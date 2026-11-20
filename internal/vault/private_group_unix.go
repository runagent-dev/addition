//go:build unix

package vault

import "os/user"

// accountSources is where a user private group's sole membership is looked up; tests swap in fixtures.
type accountSources struct {
	groupFile, passwdFile string
	currentUser           func() (*user.User, error)
	lookupGroupID         func(gid string) (*user.Group, error)
}

var accounts = accountSources{
	groupFile:     "/etc/group",
	passwdFile:    "/etc/passwd",
	currentUser:   user.Current,
	lookupGroupID: user.LookupGroupId,
}
