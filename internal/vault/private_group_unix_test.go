//go:build unix

package vault_test

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

const testUser = "addition-tester"

// accountFixture describes the account databases a private-group check reads.
type accountFixture struct {
	groupMembers string
	otherPrimary bool
	groupName    string
	userGID      func(dirGID uint32) uint32
	noGroupFile  bool
	extraGroup   string // appended to the group file; %d is the ancestor's gid
	extraPasswd  string // appended to the passwd file; %d is the ancestor's gid
}

// groupWritableHome points f at a home below a new ancestor of mode, whose group the fixture describes.
func groupWritableHome(t *testing.T, f *fakeEnv, mode os.FileMode, fx accountFixture) (ancestor string) {
	t.Helper()
	ancestor = filepath.Join(privateTempDir(t), "shared")
	if err := os.Mkdir(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	gid := info.Sys().(*syscall.Stat_t).Gid
	userGID := gid
	if fx.userGID != nil {
		userGID = fx.userGID(gid)
	}
	if fx.groupName == "" {
		fx.groupName = testUser
	}
	uid := os.Geteuid()

	files := privateTempDir(t)
	groupFile := filepath.Join(files, "group")
	if !fx.noGroupFile {
		group := fmt.Sprintf("root:x:0:\n%s:x:%d:%s\n", fx.groupName, gid, fx.groupMembers)
		if gid == 0 {
			group = fmt.Sprintf("%s:x:0:%s\n", fx.groupName, fx.groupMembers)
		}
		if fx.extraGroup != "" {
			group += fmt.Sprintf(fx.extraGroup, gid) + "\n"
		}
		writeFixture(t, groupFile, group)
	}
	passwdFile := filepath.Join(files, "passwd")
	passwd := fmt.Sprintf("%s:x:%d:%d::/home/%s:/bin/sh\n", testUser, uid, userGID, testUser)
	if fx.otherPrimary {
		passwd += fmt.Sprintf("bob:x:4242:%d::/home/bob:/bin/sh\n", gid)
	}
	if fx.extraPasswd != "" {
		passwd += fmt.Sprintf(fx.extraPasswd, gid) + "\n"
	}
	writeFixture(t, passwdFile, passwd)

	current := user.User{Uid: fmt.Sprint(uid), Gid: fmt.Sprint(userGID), Username: testUser, HomeDir: "/home/" + testUser}
	t.Cleanup(vault.SetAccountSources(groupFile, passwdFile, current, fx.groupName))
	f.set("ADDITION_HOME", filepath.Join(ancestor, "home"))
	return ancestor
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptsAnAncestorWritableOnlyByTheUserPrivateGroup(t *testing.T) {
	for name, members := range map[string]string{"no members": "", "only the user": testUser} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			groupWritableHome(t, f, 0o775, accountFixture{groupMembers: members})

			mustInit(t, f, vault.InitOptions{KeyFile: true})
			mustOpen(t, f)
		})
	}
}

func TestRejectsAGroupWritableAncestorWhoseGroupIsNotPrivate(t *testing.T) {
	cases := map[string]struct {
		fx   accountFixture
		hint string
	}{
		"supplementary member":  {fx: accountFixture{groupMembers: "bob"}, hint: "chmod g-w"},
		"another primary user":  {fx: accountFixture{otherPrimary: true}, hint: "chmod g-w"},
		"group named otherwise": {fx: accountFixture{groupName: "staff"}, hint: "chmod g-w"},
		"not the primary group": {fx: accountFixture{userGID: func(gid uint32) uint32 { return gid + 1 }}, hint: "chmod g-w"},
		"unreadable group file": {fx: accountFixture{noGroupFile: true}, hint: "cannot verify"},
		"zero-padded group gid": {fx: accountFixture{extraGroup: "devs:x:0%d:bob"}, hint: "chmod g-w"},
		"malformed group entry": {fx: accountFixture{extraGroup: "devs:x:%d:bob:extra"}, hint: "cannot verify"},
		"zero-padded user gid":  {fx: accountFixture{extraPasswd: "bob:x:4242:0%d::/home/bob:/bin/sh"}, hint: "chmod g-w"},
		"malformed passwd line": {fx: accountFixture{extraPasswd: "bob:x:4242:%d::/home/bob"}, hint: "cannot verify"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			ancestor := groupWritableHome(t, f, 0o775, c.fx)

			for op, err := range bothErrors(t, f) {
				if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), ancestor) || !strings.Contains(err.Error(), c.hint) {
					t.Errorf("%s err = %v, want ErrInsecureHome naming %s with %q", op, err, ancestor, c.hint)
				}
			}
		})
	}
}

func TestRejectsAWorldWritableAncestorEvenWithAPrivateGroup(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o757} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFakeEnv(t)
			ancestor := groupWritableHome(t, f, mode, accountFixture{})

			for op, err := range bothErrors(t, f) {
				if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), ancestor) {
					t.Errorf("%s err = %v, want ErrInsecureHome naming %s", op, err, ancestor)
				}
			}
		})
	}
}
