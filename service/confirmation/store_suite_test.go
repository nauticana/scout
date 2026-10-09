package confirmation_test

import (
	"testing"

	"github.com/nauticana/scout/service/confirmation"
	"github.com/nauticana/scout/service/confirmation/confirmationtest"
)

func TestTableStoreConformance(t *testing.T) {
	confirmationtest.RunStoreSuite(t, func(*testing.T) confirmationtest.Harness {
		store, advance := confirmation.NewFakeTableStore()
		return confirmationtest.Harness{Store: store, Prepare: store.Prepare, MarkLapsed: store.MarkLapsed, Advance: advance}
	})
}
