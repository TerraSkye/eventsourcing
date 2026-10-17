package memory_test

import (
	"testing"

	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

func TestStoreAcceptance(t *testing.T) {
	projectiontest.StoreAcceptanceTest(t, memory.NewProjectionStore())
}
