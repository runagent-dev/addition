package opsearch_test

import (
	"slices"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/opsearch"
	"github.com/runagent-dev/addition/shared"
)

func op(id, summary string, effect catalog.Effect, method, path string, params ...catalog.OperationParam) catalog.Operation {
	return catalog.Operation{ID: id, Summary: summary, Method: method, Path: path, Effect: effect, Params: params}
}

func fakeSets() []*catalog.OperationSet {
	return []*catalog.OperationSet{
		{Provider: "mailbox", Operations: []catalog.Operation{
			op("mail.messages.send", "Sends a message to the recipients.", catalog.EffectWrite, "POST", "/v1/messages/send"),
			op("mail.messages.list", "Lists the messages in the mailbox.", catalog.EffectRead, "GET", "/v1/messages"),
			op("mail.messages.delete", "Deletes a message permanently.", catalog.EffectDestructive, "DELETE", "/v1/messages/{id}",
				catalog.OperationParam{Name: "id", In: "path", Type: "string", Required: true}),
		}},
		{Provider: "files", Operations: []catalog.Operation{
			op("files.files.list", "Lists files and folders.", catalog.EffectRead, "GET", "/v1/files"),
			op("files.files.messages", "Lists messages about a file.", catalog.EffectRead, "GET", "/v1/files/messages"),
		}},
	}
}

func ids(hits []opsearch.Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Op.ID)
	}
	return out
}

func TestSearchRanksTheBestMatchFirst(t *testing.T) {
	ix := opsearch.New(fakeSets())
	hits, total := ix.Search("send a message", opsearch.Options{})
	if len(hits) == 0 || hits[0].Op.ID != "mail.messages.send" {
		t.Fatalf("hits = %v, want mail.messages.send first", ids(hits))
	}
	if total != len(hits) {
		t.Errorf("total = %d, hits = %d", total, len(hits))
	}
	if hits[0].App != "mailbox" {
		t.Errorf("app = %q, want the provider id", hits[0].App)
	}
}

func TestAnExactIDOrIDSuffixWins(t *testing.T) {
	ix := opsearch.New(fakeSets())
	for _, q := range []string{"mail.messages.list", "messages.list", "MAIL.MESSAGES.LIST"} {
		hits, _ := ix.Search(q, opsearch.Options{})
		if len(hits) == 0 || hits[0].Op.ID != "mail.messages.list" {
			t.Errorf("%q → %v, want mail.messages.list first", q, ids(hits))
		}
	}
}

func TestAppNameInTheQueryNarrowsToThatApp(t *testing.T) {
	ix := opsearch.New(fakeSets())
	hits, _ := ix.Search("files messages", opsearch.Options{})
	if len(hits) == 0 || hits[0].Op.ID != "files.files.messages" {
		t.Errorf("hits = %v", ids(hits))
	}
}

func TestFiltersAndLimit(t *testing.T) {
	ix := opsearch.New(fakeSets())
	hits, total := ix.Search("messages", opsearch.Options{App: "mailbox"})
	for _, h := range hits {
		if h.App != "mailbox" {
			t.Errorf("app filter leaked %s", h.Op.ID)
		}
	}
	if total != 3 {
		t.Errorf("total with the app filter = %d, want 3", total)
	}
	hits, _ = ix.Search("messages", opsearch.Options{Effect: catalog.EffectDestructive})
	if !slices.Equal(ids(hits), []string{"mail.messages.delete"}) {
		t.Errorf("effect filter = %v", ids(hits))
	}
	hits, total = ix.Search("messages", opsearch.Options{Limit: 1})
	if len(hits) != 1 || total < 2 {
		t.Errorf("limit: %d hits of %d", len(hits), total)
	}
}

func TestNoMatchIsEmptyNotAnError(t *testing.T) {
	hits, total := opsearch.New(fakeSets()).Search("zzzz qqqq", opsearch.Options{})
	if len(hits) != 0 || total != 0 {
		t.Errorf("hits = %v total = %d", ids(hits), total)
	}
}

func TestSearchIsDeterministic(t *testing.T) {
	ix := opsearch.New(fakeSets())
	a, _ := ix.Search("messages", opsearch.Options{})
	b, _ := ix.Search("messages", opsearch.Options{})
	if !slices.Equal(ids(a), ids(b)) {
		t.Errorf("%v vs %v", ids(a), ids(b))
	}
}

func TestFindAndNearest(t *testing.T) {
	ix := opsearch.New(fakeSets())
	e, ok := ix.Find("mail.messages.send")
	if !ok || e.App != "mailbox" || e.Op.Method != "POST" {
		t.Errorf("Find = %+v %v", e, ok)
	}
	if _, ok := ix.Find("nope"); ok {
		t.Error("Find invented an operation")
	}
	near := ix.Nearest("mail.message.sned", 2)
	if len(near) == 0 || near[0] != "mail.messages.send" {
		t.Errorf("Nearest = %v, want mail.messages.send first", near)
	}
}

func TestApps(t *testing.T) {
	apps := opsearch.New(fakeSets()).Apps()
	if len(apps) != 2 || apps[0].App != "files" || apps[1].App != "mailbox" {
		t.Fatalf("apps = %+v", apps)
	}
	if apps[1].Operations != 3 || apps[1].Effects[catalog.EffectDestructive] != 1 || apps[1].Effects[catalog.EffectRead] != 1 {
		t.Errorf("mailbox summary = %+v", apps[1])
	}
}

// Realistic intents an agent would type, against the shipped Google operations.
func TestShippedIntentsFindTheRightOperation(t *testing.T) {
	c, err := catalog.Load(shared.FS)
	if err != nil {
		t.Fatal(err)
	}
	var sets []*catalog.OperationSet
	for _, id := range c.OperationProviders() {
		set, _, err := c.Operations(id)
		if err != nil {
			t.Fatal(err)
		}
		sets = append(sets, set)
	}
	ix := opsearch.New(sets)
	for query, want := range map[string]string{
		"send an email":                    "gmail.users.messages.send",
		"search my mail":                   "gmail.users.messages.list",
		"read a message":                   "gmail.users.messages.get",
		"download an email attachment":     "gmail.users.messages.attachments.get",
		"list gmail labels":                "gmail.users.labels.list",
		"create a draft":                   "gmail.users.drafts.create",
		"move a message to trash":          "gmail.users.messages.trash",
		"list calendar events":             "calendar.events.list",
		"create a calendar event":          "calendar.events.insert",
		"am i free busy":                   "calendar.freebusy.query",
		"list my calendars":                "calendar.calendarList.list",
		"find files in drive":              "drive.files.list",
		"upload a file to drive":           "drive.files.create",
		"get file metadata":                "drive.files.get",
		"share a drive file":               "drive.permissions.create",
		"delete a drive file":              "drive.files.delete",
		"list chat spaces":                 "chat.spaces.list",
		"post a message in a chat space":   "chat.spaces.messages.create",
		"read a google doc":                "docs.documents.get",
		"edit a google doc":                "docs.documents.batchUpdate",
		"create a new document":            "docs.documents.create",
		"drive.files.export":               "drive.files.export",
		"export a google doc as pdf":       "drive.files.export",
		"list members of a chat space":     "chat.spaces.members.list",
	} {
		hits, _ := ix.Search(query, opsearch.Options{Limit: 5})
		if !slices.Contains(ids(hits), want) {
			t.Errorf("%q: top 5 = %v, want %s", query, ids(hits), want)
		}
	}
}
