package inv

import (
	"testing"

	"github.com/df-mc/dragonfly/server/session"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

func TestItemStackRequestContainerIDs(t *testing.T) {
	tests := []struct {
		name         string
		menu         bool
		source       byte
		destination  byte
		wantSource   byte
		transferOnly bool
	}{
		{"cursor", true, protocol.ContainerCursor, protocol.ContainerCursor, protocol.ContainerCursor, false},
		{"hotbar", true, protocol.ContainerHotBar, protocol.ContainerHotBar, protocol.ContainerHotBar, false},
		{"inventory", true, protocol.ContainerInventory, protocol.ContainerInventory, protocol.ContainerInventory, false},
		{"combined inventory", true, protocol.ContainerCombinedHotBarAndInventory, protocol.ContainerCombinedHotBarAndInventory, protocol.ContainerCombinedHotBarAndInventory, false},
		{"menu container", true, protocol.ContainerBarrel, protocol.ContainerBarrel, protocol.ContainerLevelEntity, false},
		{"no menu", false, protocol.ContainerBarrel, protocol.ContainerBarrel, protocol.ContainerBarrel, false},
		{"between containers", true, protocol.ContainerBarrel, protocol.ContainerInventory, protocol.ContainerBarrel, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &session.Session{}
			if test.menu {
				menuMu.Lock()
				lastMenus[s] = Menu{}
				menuMu.Unlock()
				t.Cleanup(func() {
					menuMu.Lock()
					delete(lastMenus, s)
					menuMu.Unlock()
				})
			}
			for _, factory := range stackRequestActionFactories() {
				if test.transferOnly && factory.name == "drop" {
					continue
				}
				t.Run(factory.name, func(t *testing.T) {
					source := protocol.StackRequestSlotInfo{
						Container: protocol.FullContainerName{ContainerID: test.source},
						Slot:      15, StackNetworkID: 123,
					}
					destination := protocol.StackRequestSlotInfo{
						Container: protocol.FullContainerName{ContainerID: test.destination},
						Slot:      24, StackNetworkID: 456,
					}
					action, actualSource, actualDestination := factory.make(source, destination)
					handleItemStackRequest(s, []protocol.ItemStackRequest{{Actions: []protocol.StackRequestAction{action}}})
					wantSource := source
					wantSource.Container.ContainerID = test.wantSource
					if *actualSource != wantSource {
						t.Fatalf("source = %+v, want %+v", *actualSource, wantSource)
					}
					if actualDestination != nil && *actualDestination != destination {
						t.Fatalf("destination = %+v, want %+v", *actualDestination, destination)
					}
				})
			}
		})
	}
}

type stackRequestActionFactory struct {
	name string
	make func(protocol.StackRequestSlotInfo, protocol.StackRequestSlotInfo) (protocol.StackRequestAction, *protocol.StackRequestSlotInfo, *protocol.StackRequestSlotInfo)
}

func stackRequestActionFactories() []stackRequestActionFactory {
	return []stackRequestActionFactory{
		{"take", func(source, destination protocol.StackRequestSlotInfo) (protocol.StackRequestAction, *protocol.StackRequestSlotInfo, *protocol.StackRequestSlotInfo) {
			action := &protocol.TakeStackRequestAction{}
			action.Source, action.Destination, action.Count = source, destination, 1
			return action, &action.Source, &action.Destination
		}},
		{"place", func(source, destination protocol.StackRequestSlotInfo) (protocol.StackRequestAction, *protocol.StackRequestSlotInfo, *protocol.StackRequestSlotInfo) {
			action := &protocol.PlaceStackRequestAction{}
			action.Source, action.Destination, action.Count = source, destination, 1
			return action, &action.Source, &action.Destination
		}},
		{"swap", func(source, destination protocol.StackRequestSlotInfo) (protocol.StackRequestAction, *protocol.StackRequestSlotInfo, *protocol.StackRequestSlotInfo) {
			action := &protocol.SwapStackRequestAction{Source: source, Destination: destination}
			return action, &action.Source, &action.Destination
		}},
		{"drop", func(source, _ protocol.StackRequestSlotInfo) (protocol.StackRequestAction, *protocol.StackRequestSlotInfo, *protocol.StackRequestSlotInfo) {
			action := &protocol.DropStackRequestAction{Source: source, Count: 1}
			return action, &action.Source, nil
		}},
	}
}
