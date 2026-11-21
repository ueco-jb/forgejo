// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushForceWithLease(t *testing.T) {
	ctx := t.Context()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	run := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		out, _, err := NewCommand(ctx, ToTrustedCmdArgs(args)...).RunStdString(&RunOpts{Dir: dir, Env: env})
		require.NoError(t, err, "git %v", args)
		return strings.TrimSpace(out)
	}
	commit := func(t *testing.T, dir, msg string) string {
		t.Helper()
		run(t, dir, "commit", "--allow-empty", "-m", msg)
		return run(t, dir, "rev-parse", "HEAD")
	}
	const headRef = BranchPrefix + "head"

	// setup pushes head to a fresh remote and rewrites staging so that pushing it is not a fast forward.
	setup := func(t *testing.T) (remote, local, head, rewritten string) {
		remote = filepath.Join(t.TempDir(), "remote.git")
		require.NoError(t, InitRepository(ctx, remote, true, Sha1ObjectFormat.Name()))
		// InitFull sets a global core.hooksPath to the Forgejo delegate hooks.
		require.NoError(t, os.MkdirAll(filepath.Join(remote, "hooks"), 0o755))
		run(t, remote, "config", "core.hooksPath", filepath.Join(remote, "hooks"))
		local = t.TempDir()
		run(t, local, "init", "-b", "staging")
		base := commit(t, local, "base")
		head = commit(t, local, "head")
		run(t, local, "push", remote, "staging:"+headRef)
		run(t, local, "reset", "--hard", base)
		rewritten = commit(t, local, "rebased head")
		return remote, local, head, rewritten
	}
	push := func(local, remote, head string) error {
		return Push(ctx, local, PushOptions{Remote: remote, Branch: "staging:" + headRef, ForceWithLease: headRef + ":" + head, Env: env})
	}
	hook := func(t *testing.T, remote, script string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	}

	t.Run("head unchanged", func(t *testing.T) {
		remote, local, head, rewritten := setup(t)
		require.NoError(t, push(local, remote, head))
		assert.Equal(t, rewritten, run(t, remote, "rev-parse", headRef))
	})

	t.Run("head moved while the hooks ran", func(t *testing.T) {
		remote, local, head, _ := setup(t)
		other := t.TempDir()
		run(t, other, "clone", remote, ".")
		run(t, other, "checkout", "head")
		concurrent := commit(t, other, "concurrent push")
		run(t, other, "push", "origin", "HEAD:refs/heads/side")
		hook(t, remote, "unset GIT_QUARANTINE_PATH\ngit update-ref "+headRef+" "+concurrent)

		err := push(local, remote, head)
		require.True(t, IsErrPushOutOfDate(err), "got %v", err)
		assert.Equal(t, concurrent, run(t, remote, "rev-parse", headRef))
	})

	t.Run("head deleted while the hooks ran", func(t *testing.T) {
		remote, local, head, _ := setup(t)
		hook(t, remote, "unset GIT_QUARANTINE_PATH\ngit update-ref -d "+headRef)

		err := push(local, remote, head)
		require.True(t, IsErrPushOutOfDate(err), "got %v", err)
	})

	t.Run("hook rejects the push", func(t *testing.T) {
		remote, local, head, _ := setup(t)
		hook(t, remote, "echo 'rejected (stale info) (incorrect old value provided)' >&2\nexit 1")

		err := push(local, remote, head)
		require.True(t, IsErrPushRejected(err), "got %v", err)
		assert.Equal(t, head, run(t, remote, "rev-parse", headRef))
	})

	t.Run("remote refuses non fast forwards", func(t *testing.T) {
		remote, local, head, _ := setup(t)
		run(t, remote, "config", "receive.denyNonFastForwards", "true")

		err := push(local, remote, head)
		require.True(t, IsErrPushRejected(err), "got %v", err)
		assert.Equal(t, head, run(t, remote, "rev-parse", headRef))
	})
}

func TestRefMovedDuringPush(t *testing.T) {
	const headRef = BranchPrefix + "head"
	for stderr, want := range map[string]bool{
		"remote: error: cannot lock ref 'refs/heads/head': is at 1111 but expected 2222        \n":                                   true,
		"remote: error: cannot lock ref 'refs/heads/head': reference is missing but expected 2222\n":                                 true,
		"remote: error: cannot lock ref 'refs/heads/head': Unable to create '/r.git/./refs/heads/head.lock': File exists.        \n": false,
		"remote: error: cannot lock ref 'refs/heads/other': is at 1111 but expected 2222\n":                                          false,
		"remote: hook says: cannot lock ref 'refs/heads/head' but expected\n":                                                        false,
	} {
		assert.Equal(t, want, refMovedDuringPush(stderr, headRef), stderr)
	}
}
