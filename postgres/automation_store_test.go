//go:build integration

package postgres_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/postgres"
)

func TestAutomationStoreAcceptance(t *testing.T) {
	automationtest.StoreAcceptanceTest(t, postgres.NewAutomationStore(newPool(t)))
}
