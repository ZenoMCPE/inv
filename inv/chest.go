package inv

import (
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/sound"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type chestMenu struct {
	title     string
	keepOpen  []int
	positions []cube.Pos
	actorID   uint64
	submit    func(session.Controllable, int, *world.Tx)
}

// OpenChestMenu presents a private chest without placing a block in the world.
// All inventory transactions are rejected while it is open. Valid clicks only
// invoke submit, synchronously in the transaction handling the click.
func (s *chestSession) OpenChestMenu(tx *world.Tx, c session.Controllable, title string, slots []item.Stack, keepOpen []int, submit func(session.Controllable, int, *world.Tx)) {
	if len(slots) == CompactChestSlots {
		s.openActorChestMenu(tx, c, title, slots, keepOpen, submit)
		return
	}
	if len(slots) != 27 && len(slots) != 54 {
		panic("chest menu requires 27, 45 or 54 slots")
	}
	if current := s.chestMenu; current != nil && s.openedWindow.Load().Size() == len(slots) {
		inv := s.openedWindow.Load()
		for slot, stack := range slots {
			_ = inv.SetItem(slot, stack)
		}
		s.chestMenu = &chestMenu{title: title, keepOpen: slices.Clone(keepOpen), positions: current.positions, submit: submit}
		if current.title != title {
			s.sendChestMenuBlocks(title, current.positions)
		}
		session_sendInv(s.Session, inv, s.openedWindowID.Load())
		return
	}
	s.CloseChestMenu(tx)
	chestSessions.Store(s.Session, s)
	CloseContainer(c.(*player.Player))
	session_closeCurrentContainer(s.Session, tx, false)
	c.MoveItemsToInventory()
	pos := cube.PosFromVec3(c.Position())
	pos[1] = max(tx.Range()[0], pos[1]-2)
	positions := []cube.Pos{pos}
	if len(slots) == 54 {
		positions = append(positions, pos.Add(cube.Pos{1, 0, 0}))
	}
	inv := inventory.New(len(slots), nil)
	for slot, stack := range slots {
		_ = inv.SetItem(slot, stack)
	}
	s.chestMenu = &chestMenu{title: title, keepOpen: slices.Clone(keepOpen), positions: positions, submit: submit}
	windowID := session_nextWindowID(s.Session)
	s.containerOpened.Store(true)
	s.openedContainerID.Store(protocol.ContainerTypeContainer)
	s.openedWindow.Store(inv)
	s.openedPos.Store(&pos)
	s.sendChestMenuBlocks(title, positions)
	session_writePacket(s.Session, &packet.ContainerOpen{
		WindowID: windowID, ContainerType: protocol.ContainerTypeContainer,
		ContainerPosition: protocol.BlockPos{int32(pos[0]), int32(pos[1]), int32(pos[2])}, ContainerEntityUniqueID: -1,
	})
	session_sendInv(s.Session, inv, uint32(windowID))
}

func (s *chestSession) sendChestMenuBlocks(title string, positions []cube.Pos) {
	for i, position := range positions {
		chest := block.NewChest()
		chest.CustomName = title
		s.ViewBlockUpdate(position, chest, 0)
		data := chest.EncodeNBT()
		data["x"], data["y"], data["z"] = int32(position[0]), int32(position[1]), int32(position[2])
		if len(positions) == 2 {
			pair := positions[1-i]
			data["pairx"], data["pairz"] = int32(pair[0]), int32(pair[2])
			data["pairlead"] = byte(1 - i)
		}
		session_writePacket(s.Session, &packet.BlockActorData{Position: protocol.BlockPos{int32(position[0]), int32(position[1]), int32(position[2])}, NBTData: data})
	}
}

// CloseChestMenu closes only a virtual menu, leaving regular containers alone.
func (s *chestSession) CloseChestMenu(tx *world.Tx) {
	if s.chestMenu != nil {
		s.closeChestMenu(tx, false)
	}
}

func (s *chestSession) closeChestMenu(tx *world.Tx, clientRequested bool) {
	menu := s.chestMenu
	s.chestMenu = nil
	chestSessions.Delete(s.Session)
	inv := s.openedWindow.Load()
	session_closeWindow(s.Session, clientRequested)
	if menu.actorID != 0 {
		s.removeChestActor(menu.actorID)
	}
	for _, pos := range menu.positions {
		s.ViewBlockUpdate(pos, tx.Block(pos), 0)
	}
	_ = inv.Close()
}

func (s *chestSession) chestSlot(info protocol.StackRequestSlotInfo) (int, bool) {
	if info.Container.ContainerID != protocol.ContainerLevelEntity {
		return 0, false
	}
	stack, err := s.openedWindow.Load().Item(int(info.Slot))
	return int(info.Slot), err == nil && !stack.Empty() && info.StackNetworkID == chestItemID(stack)
}

func (s *chestSession) handleChestRequest(req protocol.ItemStackRequest, tx *world.Tx, c session.Controllable) {
	slot, selected := 0, false
	for _, action := range req.Actions {
		var source protocol.StackRequestSlotInfo
		switch a := action.(type) {
		case *protocol.TakeStackRequestAction:
			source = a.Source
		case *protocol.PlaceStackRequestAction:
			source = a.Source
		case *protocol.SwapStackRequestAction:
			if _, ok := s.chestSlot(a.Source); ok {
				source = a.Source
			} else {
				source = a.Destination
			}
		default:
			continue
		}
		if slot, selected = s.chestSlot(source); selected {
			break
		}
	}
	session_writePacket(s.Session, &packet.ItemStackResponse{Responses: []protocol.ItemStackResponse{{Status: protocol.ItemStackResponseStatusError, RequestID: req.RequestID}}})
	s.finishChestClick(tx, c, slot, selected)
}

// Older clients send ordinary inventory transactions instead of stack requests.
func (s *chestSession) handleChestTransaction(pk *packet.InventoryTransaction, tx *world.Tx, c session.Controllable) {
	slot, selected := 0, false
	if _, normal := pk.TransactionData.(*protocol.NormalTransactionData); normal {
		for _, action := range pk.Actions {
			windowID, ok := action.WindowID.Value()
			if !ok || action.SourceType != protocol.InventoryActionSourceContainer || byte(windowID) != byte(s.openedWindowID.Load()) {
				continue
			}
			stack, err := s.openedWindow.Load().Item(int(action.InventorySlot))
			if err == nil && !stack.Empty() && chestStackFromItem(s.br, stack).NetworkID == action.OldItem.Stack.NetworkID &&
				chestStackFromItem(s.br, stack).MetadataValue == action.OldItem.Stack.MetadataValue {
				slot, selected = int(action.InventorySlot), true
				break
			}
		}
	}
	s.finishChestClick(tx, c, slot, selected)
}

func (s *chestSession) finishChestClick(tx *world.Tx, c session.Controllable, slot int, selected bool) {
	session_sendInv(s.Session, s.inv, protocol.WindowIDInventory)
	session_sendInv(s.Session, s.ui, protocol.WindowIDUI)
	session_sendInv(s.Session, s.offHand, protocol.WindowIDOffHand)
	session_sendInv(s.Session, s.armour.Inventory(), protocol.WindowIDArmour)
	if !selected {
		session_sendInv(s.Session, s.openedWindow.Load(), s.openedWindowID.Load())
		return
	}
	s.PlaySound(sound.Custom{Name: "random.click", Volume: 0.4, Pitch: 1}, c.Position())
	menu := s.chestMenu
	menu.submit(c, slot, tx)
	// Navigation replaces the menu in place. Teleports and other UIs may
	// already have closed it. Only close an unchanged menu for terminal actions.
	if s.chestMenu != menu {
		return
	}
	if slices.Contains(menu.keepOpen, slot) {
		session_sendInv(s.Session, s.openedWindow.Load(), s.openedWindowID.Load())
		return
	}
	s.closeChestMenu(tx, false)
}

// OpenChestMenu opens or refreshes a read-only 27/45/54-slot chest in the current
// player transaction. Callbacks receive the original slot index. A callback
// that opens another chest keeps the window open; terminal actions close it.
// CompactChestSlots uses an invisible actor and requires InventoryUIResourcePack.
func OpenChestMenu(p *player.Player, title string, slots []item.Stack, keepOpen []int, submit func(*player.Player, int)) {
	base := player_session(p)
	if base == session.Nop {
		return
	}
	state := chestState(base)
	state.OpenChestMenu(p.Tx(), p, title, slots, keepOpen, func(c session.Controllable, slot int, _ *world.Tx) { submit(c.(*player.Player), slot) })
}

// CloseChestMenu closes a virtual chest before teleporting, changing worlds,
// or opening another UI. Call it from the current player transaction.
func CloseChestMenu(p *player.Player) {
	if state, ok := chestSessions.Load(player_session(p)); ok {
		state.(*chestSession).CloseChestMenu(p.Tx())
	}
}

type chestSession struct {
	*session.Session
	chestMenu                         *chestMenu
	openedWindow                      *atomic.Pointer[inventory.Inventory]
	openedPos                         *atomic.Pointer[cube.Pos]
	openedWindowID, openedContainerID *atomic.Uint32
	containerOpened                   *atomic.Bool
	inv, ui, offHand                  *inventory.Inventory
	armour                            *inventory.Armour
	br                                world.BlockRegistry
}

var chestSessions sync.Map

func chestState(s *session.Session) *chestSession {
	if state, ok := chestSessions.Load(s); ok {
		return state.(*chestSession)
	}
	state := &chestSession{Session: s,
		openedWindow:      privateFieldPointer[atomic.Pointer[inventory.Inventory]](s, "openedWindow"),
		openedPos:         privateFieldPointer[atomic.Pointer[cube.Pos]](s, "openedPos"),
		openedWindowID:    privateFieldPointer[atomic.Uint32](s, "openedWindowID"),
		openedContainerID: privateFieldPointer[atomic.Uint32](s, "openedContainerID"),
		containerOpened:   privateFieldPointer[atomic.Bool](s, "containerOpened"),
		inv:               fetchPrivateField[*inventory.Inventory](s, "inv"),
		ui:                fetchPrivateField[*inventory.Inventory](s, "ui"),
		offHand:           fetchPrivateField[*inventory.Inventory](s, "offHand"),
		armour:            fetchPrivateField[*inventory.Armour](s, "armour"),
		br:                fetchPrivateField[world.BlockRegistry](s, "br"),
	}
	chestSessions.Store(s, state)
	return state
}

func privateFieldPointer[T any](s *session.Session, name string) *T {
	return (*T)(unsafe.Pointer(reflect.ValueOf(s).Elem().FieldByName(name).UnsafeAddr()))
}

//go:linkname session_closeCurrentContainer github.com/df-mc/dragonfly/server/session.(*Session).closeCurrentContainer
func session_closeCurrentContainer(*session.Session, *world.Tx, bool)

//go:linkname session_closeWindow github.com/df-mc/dragonfly/server/session.(*Session).closeWindow
func session_closeWindow(*session.Session, bool) bool

//go:linkname chestItemID github.com/df-mc/dragonfly/server/item.id
func chestItemID(item.Stack) int32

//go:linkname chestStackFromItem github.com/df-mc/dragonfly/server/session.stackFromItem
func chestStackFromItem(world.BlockRegistry, item.Stack) protocol.ItemStack
