package sqlite_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/sqlite"
)

func TestAutomationStoreAcceptance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			automationtest.StoreAcceptanceTest(t, sqlite.NewAutomationStore(openDB(t, d.path(t))))
		})
	}
}
