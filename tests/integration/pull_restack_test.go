// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auth_model "forgejo.org/models/auth"
	"forgejo.org/models/db"
	git_model "forgejo.org/models/git"
	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	webhook_model "forgejo.org/models/webhook"
	"forgejo.org/modules/git"
	"forgejo.org/modules/gitrepo"
	"forgejo.org/modules/json"
	"forgejo.org/modules/queue"
	repo_module "forgejo.org/modules/repository"
	api "forgejo.org/modules/structs"
	"forgejo.org/modules/test"
	webhook_module "forgejo.org/modules/webhook"
	notify_service "forgejo.org/services/notify"
	pull_service "forgejo.org/services/pull"
	repo_service "forgejo.org/services/repository"
	files_service "forgejo.org/services/repository/files"
	"forgejo.org/tests/forgery"

	"github.com/hashicorp/go-version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitFile creates or updates (op) path with content on newBranch, forked from oldBranch, and returns the new head.
func commitFile(t *testing.T, repo *repo_model.Repository, doer *user_model.User, oldBranch, newBranch, op, path, content string) string {
	t.Helper()
	gitRepo, err := gitrepo.OpenRepository(t.Context(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()
	lastCommit, err := gitRepo.GetBranchCommitID(oldBranch)
	require.NoError(t, err)

	resp, err := files_service.ChangeRepoFiles(git.DefaultContext, repo, doer, &files_service.ChangeRepoFilesOptions{
		LastCommitID: lastCommit,
		Files: []*files_service.ChangeRepoFile{{
			Operation:     op,
			TreePath:      path,
			ContentReader: strings.NewReader(content),
		}},
		Message:   "write " + path,
		OldBranch: oldBranch,
		NewBranch: newBranch,
		Author:    &files_service.IdentityOptions{Name: doer.Name, Email: doer.Email},
		Committer: &files_service.IdentityOptions{Name: doer.Name, Email: doer.Email},
		Dates:     &files_service.CommitDateOptions{Author: time.Now(), Committer: time.Now()},
	})
	require.NoError(t, err)
	return resp.Commit.SHA
}

func createPullBetween(t *testing.T, repo *repo_model.Repository, doer *user_model.User, base, head string) *issues_model.PullRequest {
	t.Helper()
	issue := &issues_model.Issue{RepoID: repo.ID, Title: head + " into " + base, PosterID: doer.ID, Poster: doer, IsPull: true}
	pr := &issues_model.PullRequest{
		HeadRepoID: repo.ID, BaseRepoID: repo.ID,
		HeadBranch: head, BaseBranch: base,
		HeadRepo: repo, BaseRepo: repo,
		Type: issues_model.PullRequestGitea,
	}
	require.NoError(t, pull_service.NewPullRequest(git.DefaultContext, repo, issue, nil, nil, pr, nil))
	return loadPull(t, pr.ID)
}

func loadPull(t *testing.T, id int64) *issues_model.PullRequest {
	t.Helper()
	pr := unittest.AssertExistsAndLoadBean(t, &issues_model.PullRequest{ID: id})
	require.NoError(t, pr.LoadIssue(t.Context()))
	require.NoError(t, pr.Issue.LoadRepo(t.Context()))
	require.NoError(t, pr.LoadBaseRepo(t.Context()))
	require.NoError(t, pr.LoadHeadRepo(t.Context()))
	return pr
}

// RebaseOnto with the old parent head as upstream keeps only the child's own commit after the parent is squash merged.
func TestPullRebaseOntoAfterSquashedParent(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		withoutReplay, err := version.NewVersion("2.43.0")
		require.NoError(t, err)
		t.Run("git rebase", func(t *testing.T) {
			defer test.MockVariableValue(&git.GitVersion, withoutReplay)()
			testRebaseOntoAfterSquashedParent(t)
		})
		t.Run("git replay", func(t *testing.T) {
			if git.CheckGitVersionAtLeast("2.54") != nil {
				t.Skip("git replay is used from git 2.54")
			}
			testRebaseOntoAfterSquashedParent(t)
		})
	})
}

func testRebaseOntoAfterSquashedParent(t *testing.T) {
	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	repo := forgery.CreateRepository(t, user, &forgery.CreateRepositoryOptions{Files: forgery.FilesInit{}})
	gitRepo, err := gitrepo.OpenRepository(t.Context(), repo)
	require.NoError(t, err)
	defer gitRepo.Close()

	commitFile(t, repo, user, "main", "parent", "create", "a.txt", "one\n")
	parentHead := commitFile(t, repo, user, "parent", "parent", "update", "a.txt", "two\n")
	childHead := commitFile(t, repo, user, "parent", "child", "create", "b.txt", "child\n")

	parentPR := createPullBetween(t, repo, user, "main", "parent")
	childPR := createPullBetween(t, repo, user, "parent", "child")

	require.NoError(t, pull_service.Merge(t.Context(), parentPR, user, gitRepo, repo_model.MergeStyleSquash, "", "squash parent", false))
	require.NoError(t, pull_service.ChangeTargetBranch(t.Context(), childPR, user, "main"))

	t.Run("upstream that is not an ancestor of the head is refused", func(t *testing.T) {
		mainHead, err := gitRepo.GetBranchCommitID("main")
		require.NoError(t, err)
		require.Error(t, pull_service.RebaseOnto(t.Context(), loadPull(t, childPR.ID), user, mainHead, childHead))
		head, err := gitRepo.GetBranchCommitID("child")
		require.NoError(t, err)
		assert.Equal(t, childHead, head)
	})

	t.Run("rebase onto with the old parent head keeps only the child's commit", func(t *testing.T) {
		require.NoError(t, pull_service.RebaseOnto(t.Context(), loadPull(t, childPR.ID), user, parentHead, childHead))

		mainHead, err := gitRepo.GetBranchCommitID("main")
		require.NoError(t, err)
		newChild, err := gitRepo.GetBranchCommit("child")
		require.NoError(t, err)
		require.Equal(t, 1, newChild.ParentCount())
		parentID, err := newChild.ParentID(0)
		require.NoError(t, err)
		assert.Equal(t, mainHead, parentID.String())

		changed, err := gitRepo.GetFilesChangedBetween("main", "child")
		require.NoError(t, err)
		assert.Equal(t, []string{"b.txt"}, changed)
	})

	t.Run("a head already on its base is left as is", func(t *testing.T) {
		mainHead, err := gitRepo.GetBranchCommitID("main")
		require.NoError(t, err)
		head, err := gitRepo.GetBranchCommitID("child")
		require.NoError(t, err)

		require.NoError(t, pull_service.RebaseOnto(t.Context(), loadPull(t, childPR.ID), user, mainHead, head))

		after, err := gitRepo.GetBranchCommitID("child")
		require.NoError(t, err)
		assert.Equal(t, head, after)
	})

	t.Run("nothing after the upstream leaves the head at the base", func(t *testing.T) {
		require.NoError(t, repo_service.CreateNewBranch(t.Context(), user, repo, gitRepo, "child", "solo"))
		soloHead, err := gitRepo.GetBranchCommitID("solo")
		require.NoError(t, err)
		soloPR := createPullBetween(t, repo, user, "main", "solo")

		require.NoError(t, pull_service.RebaseOnto(t.Context(), loadPull(t, soloPR.ID), user, soloHead, soloHead))

		mainHead, err := gitRepo.GetBranchCommitID("main")
		require.NoError(t, err)
		head, err := gitRepo.GetBranchCommitID("solo")
		require.NoError(t, err)
		assert.Equal(t, mainHead, head)
	})
}

// A push that moves the head while the server pushes the rebased head makes the update API answer 409.
func TestAPIPullUpdateByRebaseLostLease(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, _ *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := forgery.CreateRepository(t, user, &forgery.CreateRepositoryOptions{Files: forgery.FilesInit{}})
		gitRepo, err := gitrepo.OpenRepository(t.Context(), repo)
		require.NoError(t, err)
		defer gitRepo.Close()

		commitFile(t, repo, user, "main", "feature", "create", "f.txt", "feature\n")
		concurrent := commitFile(t, repo, user, "feature", "concurrent", "create", "c.txt", "concurrent\n")
		commitFile(t, repo, user, "main", "main", "create", "m.txt", "main\n")
		pr := createPullBetween(t, repo, user, "main", "feature")

		hooks := filepath.Join(repo.RepoPath(), "hooks", "pre-receive.d")
		require.NoError(t, os.MkdirAll(hooks, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hooks, "move-feature"), []byte("#!/bin/sh\nunset GIT_QUARANTINE_PATH\ngit update-ref refs/heads/feature "+concurrent+"\n"), 0o755))

		token := getTokenForLoggedInUser(t, loginUser(t, user.Name), auth_model.AccessTokenScopeWriteRepository)
		req := NewRequestf(t, "POST", "/api/v1/repos/%s/%s/pulls/%d/update?style=rebase", user.Name, repo.Name, pr.Index).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusConflict)

		head, err := gitRepo.GetBranchCommitID("feature")
		require.NoError(t, err)
		assert.Equal(t, concurrent, head)
	})
}

type retargetRecorder struct {
	notify_service.NullNotifier
	mu    sync.Mutex
	moves map[int64]string // old base branch by pull request ID
}

func (r *retargetRecorder) PullRequestChangeTargetBranch(_ context.Context, _ *user_model.User, pr *issues_model.PullRequest, oldBranch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.moves[pr.ID] = oldBranch
}

func (r *retargetRecorder) get(id int64) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.moves[id]
	return old, ok
}

// Deleting the head branch of a merged pull request outside "delete branch after merge" retargets its children.
func TestPullRetargetChildOnDeleteOfMergedBranch(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, u *url.URL) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
		repo := forgery.CreateRepository(t, user, &forgery.CreateRepositoryOptions{Files: forgery.FilesInit{}})
		gitRepo, err := gitrepo.OpenRepository(t.Context(), repo)
		require.NoError(t, err)
		defer gitRepo.Close()

		recorder := &retargetRecorder{moves: map[int64]string{}}
		notify_service.RegisterNotifier(recorder)
		defer notify_service.UnregisterNotifier(recorder)

		t.Run("merged unchanged into two branches", func(t *testing.T) {
			commitFile(t, repo, user, "main", "stable", "create", "g.txt", "stable\n")
			commitFile(t, repo, user, "main", "both", "create", "h.txt", "both\n")
			commitFile(t, repo, user, "both", "on-both", "create", "i.txt", "child\n")
			intoMain := createPullBetween(t, repo, user, "main", "both")
			intoStable := createPullBetween(t, repo, user, "stable", "both")
			childPR := createPullBetween(t, repo, user, "both", "on-both")
			require.NoError(t, pull_service.Merge(t.Context(), intoStable, user, gitRepo, repo_model.MergeStyleMerge, "", "merge both into stable", false))
			require.NoError(t, pull_service.Merge(t.Context(), intoMain, user, gitRepo, repo_model.MergeStyleMerge, "", "merge both into main", false))

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "both"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "both", child.BaseBranch)
			assert.True(t, child.Issue.IsClosed)
		})

		t.Run("merged head is deleted by a git push", func(t *testing.T) {
			commitFile(t, repo, user, "main", "pushed", "create", "v1.txt", "pushed\n")
			commitFile(t, repo, user, "pushed", "on-pushed", "create", "v2.txt", "child\n")
			pushedPR := createPullBetween(t, repo, user, "main", "pushed")
			childPR := createPullBetween(t, repo, user, "pushed", "on-pushed")
			require.NoError(t, pull_service.Merge(t.Context(), pushedPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge pushed", false))
			hook := &webhook_model.Webhook{RepoID: repo.ID, URL: "http://127.0.0.1/", ContentType: webhook_model.ContentTypeJSON, IsActive: true, Type: webhook_module.FORGEJO, HookEvent: &webhook_module.HookEvent{SendEverything: true}}
			require.NoError(t, hook.UpdateEvent())
			require.NoError(t, webhook_model.CreateWebhook(t.Context(), hook, ""))

			remote := *u
			remote.Path = repo.FullName() + ".git"
			remote.User = url.UserPassword(user.Name, userPassword)
			local := t.TempDir()
			_, _, err := git.NewCommand(t.Context(), "init").RunStdString(&git.RunOpts{Dir: local})
			require.NoError(t, err)
			_, _, err = git.NewCommand(t.Context(), "push").AddDynamicArguments(remote.String(), ":refs/heads/pushed").RunStdString(&git.RunOpts{Dir: local})
			require.NoError(t, err)
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			old, retargeted := recorder.get(childPR.ID)
			require.True(t, retargeted)
			assert.Equal(t, "pushed", old)

			// The payload permissions are those of the poster, the repo owner.
			edited := false
			for _, task := range retrieveHookTasks(t, hook.ID, false) {
				var payload api.PullRequestPayload
				require.NoError(t, json.Unmarshal([]byte(task.PayloadContent), &payload))
				if task.EventType == webhook_module.HookEventPullRequest && payload.Action == api.HookIssueEdited && payload.Index == childPR.Index {
					edited = true
					assert.True(t, payload.Repository.Permissions.Admin)
				}
			}
			assert.True(t, edited)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "main", child.BaseBranch)
			assert.False(t, child.Issue.IsClosed)
		})

		t.Run("base of the merged pull request is gone", func(t *testing.T) {
			commitFile(t, repo, user, "main", "upper", "create", "u.txt", "upper\n")
			commitFile(t, repo, user, "upper", "lower", "create", "w.txt", "lower\n")
			commitFile(t, repo, user, "lower", "on-lower", "create", "x.txt", "child\n")
			upperPR := createPullBetween(t, repo, user, "main", "upper")
			lowerPR := createPullBetween(t, repo, user, "upper", "lower")
			childPR := createPullBetween(t, repo, user, "lower", "on-lower")
			require.NoError(t, pull_service.Merge(t.Context(), lowerPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge lower", false))
			require.NoError(t, pull_service.Merge(t.Context(), loadPull(t, upperPR.ID), user, gitRepo, repo_model.MergeStyleMerge, "", "merge upper", false))
			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "upper"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "lower"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			assert.True(t, loadPull(t, childPR.ID).Issue.IsClosed)
		})

		t.Run("branch moved on after its merge", func(t *testing.T) {
			commitFile(t, repo, user, "main", "reused", "create", "c.txt", "reused\n")
			reusedPR := createPullBetween(t, repo, user, "main", "reused")
			require.NoError(t, pull_service.Merge(t.Context(), reusedPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge reused", false))
			commitFile(t, repo, user, "reused", "reused", "create", "d.txt", "after the merge\n")
			commitFile(t, repo, user, "reused", "on-reused", "create", "e.txt", "child\n")
			childPR := createPullBetween(t, repo, user, "reused", "on-reused")

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "reused"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "reused", child.BaseBranch)
			assert.True(t, child.Issue.IsClosed)
		})

		t.Run("a child that can't be retargeted stays open", func(t *testing.T) {
			commitFile(t, repo, user, "main", "ff", "create", "f.txt", "ff\n")
			// on-ff has no commits of its own, so retargeting it to main fails with branches equal.
			require.NoError(t, repo_service.CreateNewBranch(t.Context(), user, repo, gitRepo, "ff", "on-ff"))
			ffPR := createPullBetween(t, repo, user, "main", "ff")
			childPR := createPullBetween(t, repo, user, "ff", "on-ff")
			require.NoError(t, pull_service.Merge(t.Context(), ffPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge ff", false))

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "ff"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			child := loadPull(t, childPR.ID)
			assert.Equal(t, "ff", child.BaseBranch)
			assert.False(t, child.Issue.IsClosed)
		})

		t.Run("branch recreated before the deletion is handled", func(t *testing.T) {
			back := commitFile(t, repo, user, "main", "back", "create", "k.txt", "back\n")
			commitFile(t, repo, user, "back", "on-back", "create", "n.txt", "child\n")
			backPR := createPullBetween(t, repo, user, "main", "back")
			childPR := createPullBetween(t, repo, user, "back", "on-back")
			require.NoError(t, pull_service.Merge(t.Context(), backPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge back", false))

			// The deletion event of a branch that exists again.
			require.NoError(t, repo_service.PushUpdate(&repo_module.PushUpdateOptions{
				PusherID: user.ID, PusherName: user.Name, RepoUserName: repo.OwnerName, RepoName: repo.Name,
				RefFullName: git.RefNameFromBranch("back"), OldCommitID: back, NewCommitID: git.Sha1ObjectFormat.EmptyObjectID().String(),
			}))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "back", child.BaseBranch)
			assert.False(t, child.Issue.IsClosed)
		})

		t.Run("branch row lags behind the deleted tip", func(t *testing.T) {
			mergedHead := commitFile(t, repo, user, "main", "lag", "create", "w1.txt", "merged\n")
			lagPR := createPullBetween(t, repo, user, "main", "lag")
			require.NoError(t, pull_service.Merge(t.Context(), lagPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge lag", false))
			tip := commitFile(t, repo, user, "lag", "lag", "create", "w2.txt", "unmerged\n")
			commitFile(t, repo, user, "lag", "on-lag", "create", "w3.txt", "child\n")
			childPR := createPullBetween(t, repo, user, "lag", "on-lag")
			require.NoError(t, gitRepo.DeleteBranch("lag", git.DeleteBranchOptions{Force: true}))
			require.NoError(t, git_model.AddDeletedBranch(t.Context(), repo.ID, "lag", user.ID))
			// The push of tip raced the deletion, so the row still holds the merged head.
			_, err := db.GetEngine(t.Context()).Where("repo_id = ? AND name = ?", repo.ID, "lag").Cols("commit_id").Update(&git_model.Branch{CommitID: mergedHead})
			require.NoError(t, err)

			require.NoError(t, repo_service.PushUpdate(&repo_module.PushUpdateOptions{
				PusherID: user.ID, PusherName: user.Name, RepoUserName: repo.OwnerName, RepoName: repo.Name,
				RefFullName: git.RefNameFromBranch("lag"), OldCommitID: tip, NewCommitID: git.Sha1ObjectFormat.EmptyObjectID().String(),
			}))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "lag", child.BaseBranch)
			assert.True(t, child.Issue.IsClosed)
		})

		t.Run("deletion event of an older tip when the latest tip was merged", func(t *testing.T) {
			first := commitFile(t, repo, user, "main", "redo", "create", "u1.txt", "first\n")
			commitFile(t, repo, user, "redo", "redo", "create", "u2.txt", "second\n")
			redoPR := createPullBetween(t, repo, user, "main", "redo")
			commitFile(t, repo, user, "redo", "on-redo", "create", "u3.txt", "child\n")
			childPR := createPullBetween(t, repo, user, "redo", "on-redo")
			require.NoError(t, pull_service.Merge(t.Context(), redoPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge redo", false))
			require.NoError(t, gitRepo.DeleteBranch("redo", git.DeleteBranchOptions{Force: true}))
			require.NoError(t, git_model.AddDeletedBranch(t.Context(), repo.ID, "redo", user.ID))

			// An older deletion event of the branch is handled before the one of its merged tip.
			require.NoError(t, repo_service.PushUpdate(&repo_module.PushUpdateOptions{
				PusherID: user.ID, PusherName: user.Name, RepoUserName: repo.OwnerName, RepoName: repo.Name,
				RefFullName: git.RefNameFromBranch("redo"), OldCommitID: first, NewCommitID: git.Sha1ObjectFormat.EmptyObjectID().String(),
			}))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.True(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "main", child.BaseBranch)
			assert.False(t, child.Issue.IsClosed)
		})

		t.Run("deleted branch without a branch row", func(t *testing.T) {
			commitFile(t, repo, user, "main", "norow", "create", "x1.txt", "norow\n")
			commitFile(t, repo, user, "norow", "on-norow", "create", "x2.txt", "child\n")
			norowPR := createPullBetween(t, repo, user, "main", "norow")
			childPR := createPullBetween(t, repo, user, "norow", "on-norow")
			require.NoError(t, pull_service.Merge(t.Context(), norowPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge norow", false))
			_, err := db.GetEngine(t.Context()).Where("repo_id = ? AND name = ?", repo.ID, "norow").Delete(new(git_model.Branch))
			require.NoError(t, err)

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "norow"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			child := loadPull(t, childPR.ID)
			assert.Equal(t, "norow", child.BaseBranch)
			assert.False(t, child.Issue.IsClosed)
		})

		t.Run("branch still heads an open pull request", func(t *testing.T) {
			commitFile(t, repo, user, "main", "release", "create", "r.txt", "release\n")
			commitFile(t, repo, user, "main", "shared", "create", "s.txt", "shared\n")
			commitFile(t, repo, user, "shared", "on-shared", "create", "t.txt", "child\n")
			sharedPR := createPullBetween(t, repo, user, "main", "shared")
			openPR := createPullBetween(t, repo, user, "release", "shared")
			childPR := createPullBetween(t, repo, user, "shared", "on-shared")
			require.NoError(t, pull_service.Merge(t.Context(), sharedPR, user, gitRepo, repo_model.MergeStyleMerge, "", "merge shared", false))

			require.NoError(t, repo_service.DeleteBranch(t.Context(), user, repo, gitRepo, "shared"))
			require.NoError(t, queue.GetManager().FlushAll(t.Context(), 0))

			_, retargeted := recorder.get(childPR.ID)
			assert.False(t, retargeted)
			assert.True(t, loadPull(t, childPR.ID).Issue.IsClosed)
			assert.True(t, loadPull(t, openPR.ID).Issue.IsClosed)
		})
	})
}
