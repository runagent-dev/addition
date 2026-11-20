package cli

import (
	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/shared"
)

func loadCatalog() (*catalog.Catalog, error) {
	c, err := catalog.Load(shared.FS)
	if err != nil {
		return nil, &Error{Code: "catalog", Message: err.Error(), Exit: ExitInternal}
	}
	return c, nil
}
