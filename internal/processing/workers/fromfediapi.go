// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workers

import (
	"context"
	"errors"

	"codeberg.org/gruf/go-kv"
	"codeberg.org/gruf/go-logger/v2/level"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/federation/dereferencing"

	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
	"github.com/superseriousbusiness/gotosocial/internal/processing/account"
	"github.com/superseriousbusiness/gotosocial/internal/state"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// fediAPI wraps processing functions
// specifically for messages originating
// from the federation/ActivityPub API.
type fediAPI struct {
	state    *state.State
	surface  *Surface
	federate *federate
	account  *account.Processor
	utils    *utils
}

func (p *Processor) ProcessFromFediAPI(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Allocate new log fields slice
	fields := make([]kv.Field, 3, 5)
	fields[0] = kv.Field{"activityType", fMsg.APActivityType}
	fields[1] = kv.Field{"objectType", fMsg.APObjectType}
	fields[2] = kv.Field{"toAccount", fMsg.Receiving.Username}

	if fMsg.APIRI != nil {
		// An IRI was supplied, append to log
		fields = append(fields, kv.Field{
			"iri", fMsg.APIRI,
		})
	}

	// Include GTSModel in logs if appropriate.
	if fMsg.GTSModel != nil &&
		log.Level() >= level.DEBUG {
		fields = append(fields, kv.Field{
			"model", fMsg.GTSModel,
		})
	}

	l := log.WithContext(ctx).WithFields(fields...)
	l.Info("processing from fedi API")

	switch fMsg.APActivityType {

	// CREATE SOMETHING
	case ap.ActivityCreate:
		switch fMsg.APObjectType {

		// CREATE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.CreateStatus(ctx, fMsg)

		// CREATE FOLLOW (request)
		case ap.ActivityFollow:
			return p.fediAPI.CreateFollowReq(ctx, fMsg)

		// CREATE LIKE/FAVE
		case ap.ActivityLike:
			return p.fediAPI.CreateLike(ctx, fMsg)

		// CREATE ANNOUNCE/BOOST
		case ap.ActivityAnnounce:
			return p.fediAPI.CreateAnnounce(ctx, fMsg)

		// CREATE BLOCK
		case ap.ActivityBlock:
			return p.fediAPI.CreateBlock(ctx, fMsg)

		// CREATE FLAG/REPORT
		case ap.ActivityFlag:
			return p.fediAPI.CreateFlag(ctx, fMsg)

		// CREATE QUESTION
		case ap.ActivityQuestion:
			return p.fediAPI.CreatePollVote(ctx, fMsg)
		}

	// UPDATE SOMETHING
	case ap.ActivityUpdate:
		switch fMsg.APObjectType {

		// UPDATE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.UpdateStatus(ctx, fMsg)

		// UPDATE PROFILE/ACCOUNT
		case ap.ObjectProfile:
			return p.fediAPI.UpdateAccount(ctx, fMsg)
		}

	// ACCEPT SOMETHING
	case ap.ActivityAccept:
		switch fMsg.APObjectType { //nolint:gocritic

		// ACCEPT FOLLOW
		case ap.ActivityFollow:
			return p.fediAPI.AcceptFollow(ctx, fMsg)
		}

	// DELETE SOMETHING
	case ap.ActivityDelete:
		switch fMsg.APObjectType {

		// DELETE NOTE/STATUS
		case ap.ObjectNote:
			return p.fediAPI.DeleteStatus(ctx, fMsg)

		// DELETE PROFILE/ACCOUNT
		case ap.ObjectProfile:
			return p.fediAPI.DeleteAccount(ctx, fMsg)
		}

	// MOVE SOMETHING
	case ap.ActivityMove:

		// MOVE PROFILE/ACCOUNT
		// fromfediapi_move.go.
		if fMsg.APObjectType == ap.ObjectProfile {
			return p.fediAPI.MoveAccount(ctx, fMsg)
		}
	}

	return gtserror.Newf("unhandled: %s %s", fMsg.APActivityType, fMsg.APObjectType)
}

func (p *fediAPI) CreateStatus(ctx context.Context, fMsg *messages.FromFediAPI) error {
	var (
		status     *gtsmodel.Status
		statusable ap.Statusable
		err        error
	)

	var ok bool

	switch {
	case fMsg.APObject != nil:
		// A model was provided, extract this from message.
		statusable, ok = fMsg.APObject.(ap.Statusable)
		if !ok {
			return gtserror.Newf("cannot cast %T -> ap.Statusable", fMsg.APObject)
		}

		// Create bare-bones model to pass
		// into RefreshStatus(), which it will
		// further populate and insert as new.
		bareStatus := new(gtsmodel.Status)
		bareStatus.Local = util.Ptr(false)
		bareStatus.URI = ap.GetJSONLDId(statusable).String()

		// Call RefreshStatus() to parse and process the provided
		// statusable model, which it will use to further flesh out
		// the bare bones model and insert it into the database.
		status, statusable, err = p.federate.RefreshStatus(ctx,
			fMsg.Receiving.Username,
			bareStatus,
			statusable,
			// Force refresh within 5min window.
			dereferencing.Fresh,
		)
		if err != nil {
			return gtserror.Newf("error processing new status %s: %w", bareStatus.URI, err)
		}

	case fMsg.APIRI != nil:
		// Model was not set, deref with IRI (this is a forward).
		// This will also cause the status to be inserted into the db.
		status, statusable, err = p.federate.GetStatusByURI(ctx,
			fMsg.Receiving.Username,
			fMsg.APIRI,
		)
		if err != nil {
			return gtserror.Newf("error dereferencing forwarded status %s: %w", fMsg.APIRI, err)
		}

	default:
		return gtserror.New("neither APObjectModel nor APIri set")
	}

	if statusable == nil {
		// Another thread beat us to
		// creating this status! Return
		// here and let the other thread
		// handle timelining + notifying.
		return nil
	}

	// Update stats for the remote account.
	if err := p.utils.incrementStatusesCount(ctx, fMsg.Requesting, status); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status; uncache the
		// prepared version from all timelines. The status dereferencer
		// functions will ensure necessary ancestors exist before this point.
		p.surface.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	if err := p.surface.timelineAndNotifyStatus(ctx, status); err != nil {
		log.Errorf(ctx, "error timelining and notifying status: %v", err)
	}

	return nil
}

func (p *fediAPI) CreatePollVote(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Cast poll vote type from the worker message.
	vote, ok := fMsg.GTSModel.(*gtsmodel.PollVote)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.PollVote", fMsg.GTSModel)
	}

	// Insert the new poll vote in the database.
	if err := p.state.DB.PutPollVote(ctx, vote); err != nil {
		return gtserror.Newf("error inserting poll vote in db: %w", err)
	}

	// Ensure the poll vote is fully populated at this point.
	if err := p.state.DB.PopulatePollVote(ctx, vote); err != nil {
		return gtserror.Newf("error populating poll vote from db: %w", err)
	}

	// Ensure the poll on the vote is fully populated to get origin status.
	if err := p.state.DB.PopulatePoll(ctx, vote.Poll); err != nil {
		return gtserror.Newf("error populating poll from db: %w", err)
	}

	// Get the origin status,
	// (also set the poll on it).
	status := vote.Poll.Status
	status.Poll = vote.Poll

	// Interaction counts changed on the source status, uncache from timelines.
	p.surface.invalidateStatusFromTimelines(ctx, vote.Poll.StatusID)

	if *status.Local {
		// Before federating it, increment the
		// poll vote counts on our local copy.
		status.Poll.IncrementVotes(vote.Choices)

		// These were poll votes in a local status, we need to
		// federate the updated status model with latest vote counts.
		if err := p.federate.UpdateStatus(ctx, status); err != nil {
			log.Errorf(ctx, "error federating status update: %v", err)
		}
	}

	return nil
}

func (p *fediAPI) CreateFollowReq(ctx context.Context, fMsg *messages.FromFediAPI) error {
	followRequest, ok := fMsg.GTSModel.(*gtsmodel.FollowRequest)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.FollowRequest", fMsg.GTSModel)
	}

	if err := p.state.DB.PopulateFollowRequest(ctx, followRequest); err != nil {
		return gtserror.Newf("error populating follow request: %w", err)
	}

	if *followRequest.TargetAccount.Locked {
		// Local account is locked: just notify the follow request.
		if err := p.surface.notifyFollowRequest(ctx, followRequest); err != nil {
			log.Errorf(ctx, "error notifying follow request: %v", err)
		}

		// And update stats for the local account.
		if err := p.utils.incrementFollowRequestsCount(ctx, fMsg.Receiving); err != nil {
			log.Errorf(ctx, "error updating account stats: %v", err)
		}

		return nil
	}

	// Local account is not locked:
	// Automatically accept the follow request
	// and notify about the new follower.
	follow, err := p.state.DB.AcceptFollowRequest(
		ctx,
		followRequest.AccountID,
		followRequest.TargetAccountID,
	)
	if err != nil {
		return gtserror.Newf("error accepting follow request: %w", err)
	}

	// Update stats for the local account.
	if err := p.utils.incrementFollowersCount(ctx, fMsg.Receiving); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	// Update stats for the remote account.
	if err := p.utils.incrementFollowingCount(ctx, fMsg.Requesting); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	if err := p.federate.AcceptFollow(ctx, follow); err != nil {
		log.Errorf(ctx, "error federating follow request accept: %v", err)
	}

	if err := p.surface.notifyFollow(ctx, follow); err != nil {
		log.Errorf(ctx, "error notifying follow: %v", err)
	}

	return nil
}

func (p *fediAPI) CreateLike(ctx context.Context, fMsg *messages.FromFediAPI) error {
	fave, ok := fMsg.GTSModel.(*gtsmodel.StatusFave)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.StatusFave", fMsg.GTSModel)
	}

	// Ensure fave populated.
	if err := p.state.DB.PopulateStatusFave(ctx, fave); err != nil {
		return gtserror.Newf("error populating status fave: %w", err)
	}

	if err := p.surface.notifyFave(ctx, fave); err != nil {
		log.Errorf(ctx, "error notifying fave: %v", err)
	}

	// Interaction counts changed on the faved status;
	// uncache the prepared version from all timelines.
	p.surface.invalidateStatusFromTimelines(ctx, fave.StatusID)

	return nil
}

func (p *fediAPI) CreateAnnounce(ctx context.Context, fMsg *messages.FromFediAPI) error {
	boost, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Status", fMsg.GTSModel)
	}

	// Dereference status that this boosts, note
	// that this will handle storing the boost in
	// the db, and dereferencing the target status
	// ancestors / descendants where appropriate.
	var err error
	boost, err = p.federate.EnrichAnnounce(
		ctx,
		boost,
		fMsg.Receiving.Username,
	)
	if err != nil {
		if gtserror.IsUnretrievable(err) {
			// Boosted status domain blocked, nothing to do.
			log.Debugf(ctx, "skipping announce: %v", err)
			return nil
		}

		// Actual error.
		return gtserror.Newf("error dereferencing announce: %w", err)
	}

	// Update stats for the remote account.
	if err := p.utils.incrementStatusesCount(ctx, fMsg.Requesting, boost); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	// Timeline and notify the announce.
	if err := p.surface.timelineAndNotifyStatus(ctx, boost); err != nil {
		log.Errorf(ctx, "error timelining and notifying status: %v", err)
	}

	if err := p.surface.notifyAnnounce(ctx, boost); err != nil {
		log.Errorf(ctx, "error notifying announce: %v", err)
	}

	// Interaction counts changed on the original status;
	// uncache the prepared version from all timelines.
	p.surface.invalidateStatusFromTimelines(ctx, boost.BoostOfID)

	return nil
}

func (p *fediAPI) CreateBlock(ctx context.Context, fMsg *messages.FromFediAPI) error {
	block, ok := fMsg.GTSModel.(*gtsmodel.Block)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Block", fMsg.GTSModel)
	}

	// Remove each account's posts from the other's timelines.
	//
	// First home timelines.
	if err := p.state.Timelines.Home.WipeItemsFromAccountID(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from block -> target's home timeline: %v", err)
	}

	if err := p.state.Timelines.Home.WipeItemsFromAccountID(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from target -> block's home timeline: %v", err)
	}

	// Now list timelines.
	if err := p.state.Timelines.List.WipeItemsFromAccountID(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from block -> target's list timeline(s): %v", err)
	}

	if err := p.state.Timelines.List.WipeItemsFromAccountID(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error wiping items from target -> block's list timeline(s): %v", err)
	}

	// Remove any follows that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollow(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow from block -> target: %v", err)
	}

	if err := p.state.DB.DeleteFollow(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow from target -> block: %v", err)
	}

	// Remove any follow requests that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollowRequest(
		ctx,
		block.AccountID,
		block.TargetAccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow request from block -> target: %v", err)
	}

	if err := p.state.DB.DeleteFollowRequest(
		ctx,
		block.TargetAccountID,
		block.AccountID,
	); err != nil {
		log.Errorf(ctx, "error deleting follow request from target -> block: %v", err)
	}

	return nil
}

func (p *fediAPI) CreateFlag(ctx context.Context, fMsg *messages.FromFediAPI) error {
	incomingReport, ok := fMsg.GTSModel.(*gtsmodel.Report)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Report", fMsg.GTSModel)
	}

	// TODO: handle additional side effects of flag creation:
	// - notify admins by dm / notification

	if err := p.surface.emailAdminReportOpened(ctx, incomingReport); err != nil {
		log.Errorf(ctx, "error emailing report opened: %v", err)
	}

	return nil
}

func (p *fediAPI) UpdateAccount(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Parse the old/existing account model.
	account, ok := fMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.Account", fMsg.GTSModel)
	}

	// Because this was an Update, the new Accountable should be set on the message.
	apubAcc, ok := fMsg.APObject.(ap.Accountable)
	if !ok {
		return gtserror.Newf("cannot cast %T -> ap.Accountable", fMsg.APObject)
	}

	// Fetch up-to-date bio, avatar, header, etc.
	_, _, err := p.federate.RefreshAccount(
		ctx,
		fMsg.Receiving.Username,
		account,
		apubAcc,
		// Force refresh within 5min window.
		dereferencing.Fresh,
	)
	if err != nil {
		log.Errorf(ctx, "error refreshing account: %v", err)
	}

	return nil
}

func (p *fediAPI) AcceptFollow(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Update stats for the remote account.
	if err := p.utils.decrementFollowRequestsCount(ctx, fMsg.Requesting); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	if err := p.utils.incrementFollowersCount(ctx, fMsg.Requesting); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	// Update stats for the local account.
	if err := p.utils.incrementFollowingCount(ctx, fMsg.Receiving); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	return nil
}

func (p *fediAPI) UpdateStatus(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Cast the existing Status model attached to msg.
	existing, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("cannot cast %T -> *gtsmodel.Status", fMsg.GTSModel)
	}

	// Cast the updated ActivityPub statusable object .
	apStatus, _ := fMsg.APObject.(ap.Statusable)

	// Fetch up-to-date attach status attachments, etc.
	status, _, err := p.federate.RefreshStatus(
		ctx,
		fMsg.Receiving.Username,
		existing,
		apStatus,
		// Force refresh within 5min window.
		dereferencing.Fresh,
	)
	if err != nil {
		log.Errorf(ctx, "error refreshing status: %v", err)
	}

	// Status representation was refetched, uncache from timelines.
	p.surface.invalidateStatusFromTimelines(ctx, status.ID)

	if status.Poll != nil && status.Poll.Closing {

		// If the latest status has a newly closed poll, at least compared
		// to the existing version, then notify poll close to all voters.
		if err := p.surface.notifyPollClose(ctx, status); err != nil {
			log.Errorf(ctx, "error sending poll notification: %v", err)
		}
	}

	// Push message that the status has been edited to streams.
	if err := p.surface.timelineStatusUpdate(ctx, status); err != nil {
		log.Errorf(ctx, "error streaming status edit: %v", err)
	}

	return nil
}

func (p *fediAPI) DeleteStatus(ctx context.Context, fMsg *messages.FromFediAPI) error {
	// Delete attachments from this status, since this request
	// comes from the federating API, and there's no way the
	// poster can do a delete + redraft for it on our instance.
	const deleteAttachments = true

	status, ok := fMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Status", fMsg.GTSModel)
	}

	// Try to populate status structs if possible,
	// in order to more thoroughly remove them.
	if err := p.state.DB.PopulateStatus(
		ctx, status,
	); err != nil && !errors.Is(err, db.ErrNoEntries) {
		return gtserror.Newf("db error populating status: %w", err)
	}

	// Drop any outgoing queued AP requests about / targeting
	// this status, (stops queued likes, boosts, creates etc).
	p.state.Workers.Delivery.Queue.Delete("ObjectID", status.URI)
	p.state.Workers.Delivery.Queue.Delete("TargetID", status.URI)

	// Drop any incoming queued client messages about / targeting
	// status, (stops processing of local origin data for status).
	p.state.Workers.Client.Queue.Delete("TargetURI", status.URI)

	// Drop any incoming queued federator messages targeting status,
	// (stops processing of remote origin data targeting this status).
	p.state.Workers.Federator.Queue.Delete("TargetURI", status.URI)

	// First perform the actual status deletion.
	if err := p.utils.wipeStatus(ctx, status, deleteAttachments); err != nil {
		log.Errorf(ctx, "error wiping status: %v", err)
	}

	// Update stats for the remote account.
	if err := p.utils.decrementStatusesCount(ctx, fMsg.Requesting); err != nil {
		log.Errorf(ctx, "error updating account stats: %v", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.surface.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	return nil
}

func (p *fediAPI) DeleteAccount(ctx context.Context, fMsg *messages.FromFediAPI) error {
	account, ok := fMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return gtserror.Newf("%T not parseable as *gtsmodel.Account", fMsg.GTSModel)
	}

	// Drop any outgoing queued AP requests to / from / targeting
	// this account, (stops queued likes, boosts, creates etc).
	p.state.Workers.Delivery.Queue.Delete("ObjectID", account.URI)
	p.state.Workers.Delivery.Queue.Delete("TargetID", account.URI)

	// Drop any incoming queued client messages to / from this
	// account, (stops processing of local origin data for acccount).
	p.state.Workers.Client.Queue.Delete("Target.ID", account.ID)
	p.state.Workers.Client.Queue.Delete("TargetURI", account.URI)

	// Drop any incoming queued federator messages to this account,
	// (stops processing of remote origin data targeting this account).
	p.state.Workers.Federator.Queue.Delete("Requesting.ID", account.ID)
	p.state.Workers.Federator.Queue.Delete("TargetURI", account.URI)

	// First perform the actual account deletion.
	if err := p.account.Delete(ctx, account, account.ID); err != nil {
		log.Errorf(ctx, "error deleting account: %v", err)
	}

	return nil
}
