package memory_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/memory"
)

func TestAutomationStoreAcceptance(t *testing.T) {
	automationtest.StoreAcceptanceTest(t, memory.NewAutomationStore())
}
