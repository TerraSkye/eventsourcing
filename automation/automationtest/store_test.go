package automationtest_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/automation/automationtest"
)

func TestStoreAcceptance_LocalStore(t *testing.T) {
	automationtest.StoreAcceptanceTest(t, automation.NewLocalStore())
}
