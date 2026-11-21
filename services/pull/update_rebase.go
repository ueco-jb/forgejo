// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	user_model "forgejo.org/models/user"
	"forgejo.org/modules/git"
	"forgejo.org/modules/log"
	repo_module "forgejo.org/modules/repository"
	"forgejo.org/modules/setting"
)

// RebaseOnto rebases the head branch of pr onto its base branch, replaying only the commits after upstream (a full commit ID, or empty for the base branch), and pushes it with a lease on the fetched head.
// It fails with models.ErrSHADoesNotMatch if expectedHeadSHA is given and the head differs, with models.ErrUpstreamNotAncestor if upstream is not in the head, and with git.ErrPushOutOfDate if the head moves before the push.
// The caller checks IsUserAllowedToUpdate.
func RebaseOnto(ctx context.Context, pr *issues_model.PullRequest, doer *user_model.User, upstream, expectedHeadSHA string) error {
	if pr.Flow == issues_model.PullRequestFlowAGit {
		return errors.New("update of agit flow pull request's head branch is unsupported")
	}
	if upstream != "" {
		if _, err := git.NewIDFromString(upstream); err != nil {
			return fmt.Errorf("upstream is not a full commit ID: %w", err)
		}
	}

	defer pullWorkingPool.Lock(fmt.Sprint(pr.ID))()

	defer func() {
		AddTestPullRequestTask(ctx, doer, pr.BaseRepoID, pr.BaseBranch, false, "", "", 0)
	}()

	return rebaseHeadOnto(ctx, pr, doer, upstream, expectedHeadSHA)
}

// rebaseHeadOnto does the work of RebaseOnto.
// The caller holds the pull request lock.
func rebaseHeadOnto(ctx context.Context, pr *issues_model.PullRequest, doer *user_model.User, upstream, expectedHeadSHA string) error {
	// "Clone" base repo and add the cache headers for the head repo and branch
	mergeCtx, cancel, err := createTemporaryRepoForMerge(ctx, pr, doer, expectedHeadSHA)
	if err != nil {
		return err
	}
	defer cancel()

	leaseSHA, err := git.GetFullCommitID(ctx, mergeCtx.tmpBasePath, git.BranchPrefix+trackingBranch)
	if err != nil {
		return fmt.Errorf("unable to get sha of head branch in %v: %w", pr, err)
	}

	// Determine the old merge-base before the rebase - we use this for LFS push later on
	oldMergeBase, _, _ := git.NewCommand(ctx, "merge-base").AddDashesAndList(baseBranch, trackingBranch).RunStdString(&git.RunOpts{Dir: mergeCtx.tmpBasePath})
	oldMergeBase = strings.TrimSpace(oldMergeBase)

	// Rebase the tracking branch on to the base as the staging branch
	if err := rebaseTrackingOnToBase(mergeCtx, repo_model.MergeStyleRebaseUpdate, upstream); err != nil {
		return err
	}

	if setting.LFS.StartServer {
		// Now we need to ensure that the head repository contains any LFS objects between the new base and the old mergebase
		// It's questionable about where this should go - either after or before the push
		// I think in the interests of data safety - failures to push to the lfs should prevent
		// the push as you can always re-rebase.
		if err := LFSPush(ctx, mergeCtx.tmpBasePath, baseBranch, oldMergeBase, &issues_model.PullRequest{
			HeadRepoID: pr.BaseRepoID,
			BaseRepoID: pr.HeadRepoID,
		}); err != nil {
			log.Error("Unable to push lfs objects between %s and %s up to head branch in %-v: %v", baseBranch, oldMergeBase, pr, err)
			return err
		}
	}

	// Now determine who the pushing author should be
	var headUser *user_model.User
	if err := pr.HeadRepo.LoadOwner(ctx); err != nil {
		if !user_model.IsErrUserNotExist(err) {
			log.Error("Can't find user: %d for head repository in %-v - %v", pr.HeadRepo.OwnerID, pr, err)
			return err
		}
		log.Error("Can't find user: %d for head repository in %-v - defaulting to doer: %-v - %v", pr.HeadRepo.OwnerID, pr, doer, err)
		headUser = doer
	} else {
		headUser = pr.HeadRepo.Owner
	}

	if TestSeamBeforeHeadPush != nil && testing.Testing() {
		TestSeamBeforeHeadPush(pr)
	}

	// Push back to the head repository.
	// TODO: this cause an api call to "/api/internal/hook/post-receive/...",
	//       that prevents us from doint the whole merge in one db transaction
	headRef := git.BranchPrefix + pr.HeadBranch
	return git.Push(ctx, mergeCtx.tmpBasePath, git.PushOptions{
		Remote:         "head_repo",
		Branch:         stagingBranch + ":" + headRef,
		ForceWithLease: headRef + ":" + leaseSHA,
		Env:            repo_module.FullPushingEnvironment(headUser, doer, pr.HeadRepo, pr.HeadRepo.Name, pr.ID),
	})
}

// TestSeamBeforeHeadPush runs in tests after the head is rebased and before it is pushed.
var TestSeamBeforeHeadPush func(*issues_model.PullRequest)
