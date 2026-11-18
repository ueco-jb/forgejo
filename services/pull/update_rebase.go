// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"errors"
	"fmt"
	"strings"

	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	user_model "forgejo.org/models/user"
	"forgejo.org/modules/git"
	"forgejo.org/modules/log"
	repo_module "forgejo.org/modules/repository"
	"forgejo.org/modules/setting"
)

// RebaseOnto rebases the head branch of pr onto its base branch, replaying only the commits after upstream (a full commit ID, or empty for the base branch), and pushes it with a lease on the fetched head.
// It fails with models.ErrSHADoesNotMatch if expectedHeadSHA is given and the head differs, and with git.ErrPushOutOfDate if the head moves before the push.
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

// rebaseHeadOnto does the work of RebaseOnto; the caller holds the pull request lock.
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

	// Push back to the head repository.
	// TODO: this cause an api call to "/api/internal/hook/post-receive/...",
	//       that prevents us from doint the whole merge in one db transaction
	return pushHeadWithLease(ctx, mergeCtx.tmpBasePath, "head_repo", git.BranchPrefix+pr.HeadBranch, leaseSHA,
		repo_module.FullPushingEnvironment(headUser, doer, pr.HeadRepo, pr.HeadRepo.Name, pr.ID))
}

// pushHeadWithLease pushes the staging branch of the repository in dir to headRef of remote if headRef still points at leaseSHA.
func pushHeadWithLease(ctx context.Context, dir, remote, headRef, leaseSHA string, env []string) error {
	var outbuf, errbuf strings.Builder
	err := git.NewCommand(ctx, "push", "--porcelain").
		AddOptionFormat("--force-with-lease=%s:%s", headRef, leaseSHA).
		AddDynamicArguments(remote, stagingBranch+":"+headRef).
		Run(&git.RunOpts{Env: env, Dir: dir, Stdout: &outbuf, Stderr: &errbuf})
	if err == nil {
		return nil
	}

	// The porcelain status line of headRef, not hook output that may contain the same words.
	status := ""
	for line := range strings.Lines(outbuf.String()) {
		if fields := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 3); len(fields) == 3 && strings.HasSuffix(fields[1], ":"+headRef) {
			status = fields[2]
		}
	}
	switch {
	// A lease that does not hold is stale info; receive-pack reports a ref moved, deleted or locked while the hooks ran, older git only in its error text.
	case strings.HasSuffix(status, "(stale info)"), strings.HasSuffix(status, "(incorrect old value provided)"), strings.HasSuffix(status, "(reference does not exist)"),
		(strings.HasSuffix(status, "(failed to update ref)") || strings.HasSuffix(status, "(reference already exists)")) && lostRefRace(errbuf.String(), headRef):
		return &git.ErrPushOutOfDate{StdOut: outbuf.String(), StdErr: errbuf.String(), Err: err}
	case strings.HasPrefix(status, "[remote rejected]"):
		err := &git.ErrPushRejected{StdOut: outbuf.String(), StdErr: errbuf.String(), Err: err}
		err.GenerateMessage()
		return err
	}
	return fmt.Errorf("git push: %s: %w", errbuf.String(), err)
}

// lostRefRace reports whether receive-pack could not lock headRef because another writer moved or holds it.
func lostRefRace(stderr, headRef string) bool {
	for line := range strings.Lines(stderr) {
		_, reason, ok := strings.Cut(line, "error: cannot lock ref '"+headRef+"': ")
		if !ok {
			continue
		}
		if strings.Contains(reason, " but expected ") {
			return true
		}
		if strings.Contains(reason, ".lock': File exists") {
			// A lock file left by a crashed git process looks the same and blocks every later update.
			log.Warn("push to %s found the ref locked: %s", headRef, strings.TrimSpace(reason))
			return true
		}
	}
	return false
}
