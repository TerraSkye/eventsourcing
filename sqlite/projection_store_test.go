package sqlite_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/projection/projectiontest"
	"github.com/terraskye/eventsourcing/sqlite"
)

func TestProjectionStoreAcceptance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			projectiontest.StoreAcceptanceTest(t, sqlite.NewProjectionStore(openDB(t, d.path(t))))
		})
	}
}
