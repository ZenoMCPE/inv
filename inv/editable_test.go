package inv

import (
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"testing"
)

func TestEditablePlanConservesItemsAndDoesNotCommit(t *testing.T) {
	backing := inventory.New(2, nil)
	_ = backing.SetItem(0, item.NewStack(item.Diamond{}, 10))
	a, _ := backing.Item(0)
	cells := []*editableCell{{inv: backing, slot: 0, before: a, after: a}, {inv: backing, slot: 1}}
	get := func(info protocol.StackRequestSlotInfo) (*editableCell, bool) {
		if info.Slot > 1 {
			return nil, false
		}
		return cells[info.Slot], true
	}
	transfer := &protocol.TakeStackRequestAction{}
	transfer.Source.Slot, transfer.Destination.Slot, transfer.Count = 0, 1, 4
	if !planEditable(protocol.ItemStackRequest{Actions: []protocol.StackRequestAction{transfer}}, get) {
		t.Fatal("valid split rejected")
	}
	if cells[0].after.Count() != 6 || cells[1].after.Count() != 4 {
		t.Fatal("split did not conserve count")
	}
	unchanged, _ := backing.Item(0)
	empty, _ := backing.Item(1)
	if unchanged.Count() != 10 || !empty.Empty() {
		t.Fatal("planner wrote live inventory before validation")
	}
	invalid := &protocol.TakeStackRequestAction{}
	invalid.Source.Slot, invalid.Destination.Slot, invalid.Count = 1, 0, 5
	if planEditable(protocol.ItemStackRequest{Actions: []protocol.StackRequestAction{invalid}}, get) {
		t.Fatal("overdraw accepted")
	}
}

func TestEditableRejectsUnsafeActionsAndOverflow(t *testing.T) {
	for _, action := range []protocol.StackRequestAction{&protocol.DropStackRequestAction{}, &protocol.DestroyStackRequestAction{}, &protocol.CraftCreativeStackRequestAction{}} {
		if planEditable(protocol.ItemStackRequest{Actions: []protocol.StackRequestAction{action}}, nil) {
			t.Fatalf("unsafe %T accepted", action)
		}
	}
	a, b := item.NewStack(item.Diamond{}, 10), item.NewStack(item.Diamond{}, 63)
	cells := []*editableCell{{after: a}, {after: b}}
	get := func(info protocol.StackRequestSlotInfo) (*editableCell, bool) { return cells[info.Slot], true }
	transfer := &protocol.TakeStackRequestAction{}
	transfer.Source.Slot, transfer.Destination.Slot, transfer.Count = 0, 1, 2
	if planEditable(protocol.ItemStackRequest{Actions: []protocol.StackRequestAction{transfer}}, get) {
		t.Fatal("overflow accepted")
	}
	transfer.Destination.Slot = 0
	if planEditable(protocol.ItemStackRequest{Actions: []protocol.StackRequestAction{transfer}}, get) {
		t.Fatal("self-transfer accepted")
	}
}
