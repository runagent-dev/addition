//go:build unix

package vault

import "os/user"

// SetBeforeKeyPublish installs hook and returns a func restoring the previous one.
func SetBeforeKeyPublish(hook func()) (restore func()) {
	prev := beforeKeyPublish
	beforeKeyPublish = hook
	return func() { beforeKeyPublish = prev }
}

// SetAccountSources makes current the user, groupName the name of every gid, and the files the account databases.
func SetAccountSources(groupFile, passwdFile string, current user.User, groupName string) (restore func()) {
	prev := accounts
	accounts = accountSources{
		groupFile:     groupFile,
		passwdFile:    passwdFile,
		currentUser:   func() (*user.User, error) { u := current; return &u, nil },
		lookupGroupID: func(gid string) (*user.Group, error) { return &user.Group{Gid: gid, Name: groupName}, nil },
	}
	return func() { accounts = prev }
}
