// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import (
	"code.forgejo.org/xorm/xorm"
)

func init() {
	registerMigration(&Migration{
		Description: "add index to pull_request (base_repo_id, base_branch) and column merged_head_commit_id",
		Upgrade:     addPullRequestBaseIndexAndMergedHead,
	})
}

func addPullRequestBaseIndexAndMergedHead(x *xorm.Engine) error {
	type PullRequest struct {
		BaseRepoID         int64  `xorm:"INDEX INDEX(base)"`
		BaseBranch         string `xorm:"INDEX(base)"`
		MergedHeadCommitID string `xorm:"VARCHAR(64)"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, new(PullRequest))
	return err
}
