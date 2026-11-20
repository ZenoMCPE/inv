package inv

import (
	"sort"
	"time"

	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// EditableSlot binds a displayed slot to an authoritative inventory. Bindings
// are resolved afresh inside the viewer's world transaction and never retained.
// A nil Inventory displays Placeholder and cannot be edited.
type EditableSlot struct {
	Inventory   *inventory.Inventory
	Slot        int
	Placeholder item.Stack
}

// EditableSource must only return inventories owned by the current transaction.
// Resolve returning false closes the window (permission/session/world changed).
// AllowChange validates every proposed final slot before ANY slot is written.
// menuSlot is -1 for the viewer's personal inventory/cursor. Implementations
// must not mutate inventories from these callbacks or retain p/transaction data.
type EditableSource interface {
	Resolve(p *player.Player) ([]EditableSlot, bool)
	AllowChange(p *player.Player, menuSlot int, before, after item.Stack) bool
}

type editableKey struct{ container, slot byte }
type editableReceipt struct {
	at  time.Time
	ids map[editableKey]int32
}
type editableMenu struct {
	source   EditableSource
	receipts map[int32]editableReceipt
}

// OpenEditableMenu opens a 27/54-slot, opt-in live inventory view. Only explicit
// take/place/swap requests are accepted. Drop, destroy, crafting and legacy
// InventoryTransaction edits are rejected. Ordinary menus are unaffected.
// Callers must use CloseChestMenu/Forget on world transfer and disconnect.
func OpenEditableMenu(p *player.Player, title string, source EditableSource) bool {
	bindings, ok := source.Resolve(p)
	if !ok || (len(bindings) != 27 && len(bindings) != 54) || player_session(p) == session.Nop {
		return false
	}
	slots, ok := editableStacks(bindings)
	if !ok {
		return false
	}
	CloseChestMenu(p)
	OpenChestMenu(p, title, slots, nil, func(*player.Player, int) {})
	state := chestState(player_session(p))
	menu := &editableMenu{source: source, receipts: make(map[int32]editableReceipt)}
	state.chestMenu.editable = menu
	scheduleEditableRefresh(p.H(), player_session(p), menu)
	return true
}

func editableStacks(bindings []EditableSlot) ([]item.Stack, bool) {
	stacks := make([]item.Stack, len(bindings))
	for i, binding := range bindings {
		stacks[i] = binding.Placeholder
		if binding.Inventory != nil {
			var err error
			stacks[i], err = binding.Inventory.Item(binding.Slot)
			if err != nil {
				return nil, false
			}
		}
	}
	return stacks, true
}

func scheduleEditableRefresh(handle *world.EntityHandle, original *session.Session, menu *editableMenu) {
	handle.DoAfter(100*time.Millisecond, func(tx *world.Tx, entity world.Entity) {
		p, ok := entity.(*player.Player)
		if !ok || player_session(p) != original {
			return
		}
		value, ok := chestSessions.Load(original)
		if !ok {
			return
		}
		state := value.(*chestSession)
		if state.chestMenu == nil || state.chestMenu.editable != menu {
			return
		}
		bindings, valid := menu.source.Resolve(p)
		if !valid || len(bindings) != state.openedWindow.Load().Size() {
			state.closeChestMenu(tx, false)
			return
		}
		stacks, valid := editableStacks(bindings)
		if !valid {
			state.closeChestMenu(tx, false)
			return
		}
		changed := false
		for slot, stack := range stacks {
			before, _ := state.openedWindow.Load().Item(slot)
			if chestItemID(before) != chestItemID(stack) || before.Count() != stack.Count() {
				_ = state.openedWindow.Load().SetItem(slot, stack)
				changed = true
			}
		}
		if changed {
			session_sendInv(original, state.openedWindow.Load(), state.openedWindowID.Load())
		}
		scheduleEditableRefresh(handle, original, menu)
	})
}

type editableCell struct {
	predicted      bool
	inv            *inventory.Inventory
	slot, menuSlot int
	before, after  item.Stack
}

func canonicalEditableKey(info protocol.StackRequestSlotInfo) editableKey {
	container := info.Container.ContainerID
	if container == protocol.ContainerHotBar || container == protocol.ContainerInventory {
		container = protocol.ContainerCombinedHotBarAndInventory
	}
	return editableKey{container, info.Slot}
}

func (s *chestSession) handleEditableRequest(p *player.Player, req protocol.ItemStackRequest) {
	menu := s.chestMenu.editable
	bindings, valid := menu.source.Resolve(p)
	if !valid || len(bindings) != s.openedWindow.Load().Size() {
		s.rejectEditable(req.RequestID)
		s.closeChestMenu(p.Tx(), false)
		return
	}
	now := time.Now()
	for id, receipt := range menu.receipts {
		if now.Sub(receipt.at) > 5*time.Second {
			delete(menu.receipts, id)
		}
	}
	if _, duplicate := menu.receipts[req.RequestID]; duplicate || len(menu.receipts) >= 256 || len(req.Actions) == 0 || len(req.Actions) > 128 {
		s.rejectEditable(req.RequestID)
		return
	}
	cells := make(map[editableKey]*editableCell)
	get := func(info protocol.StackRequestSlotInfo) (*editableCell, bool) {
		key := canonicalEditableKey(info)
		cell, found := cells[key]
		if !found {
			cell = &editableCell{slot: int(key.slot), menuSlot: -1}
			switch key.container {
			case protocol.ContainerLevelEntity:
				if int(key.slot) >= len(bindings) {
					return nil, false
				}
				binding := bindings[key.slot]
				if binding.Inventory == nil {
					return nil, false
				}
				cell.inv, cell.slot, cell.menuSlot = binding.Inventory, binding.Slot, int(key.slot)
			case protocol.ContainerCombinedHotBarAndInventory:
				cell.inv = s.inv
			case protocol.ContainerCursor:
				if key.slot != 0 {
					return nil, false
				}
				cell.inv = s.ui
			default:
				return nil, false
			}
			// Aliased bindings would otherwise permit writing one source twice.
			for _, other := range cells {
				if cell.inv == other.inv && cell.slot == other.slot {
					return nil, false
				}
			}
			var err error
			cell.before, err = cell.inv.Item(cell.slot)
			if err != nil {
				return nil, false
			}
			cell.after = cell.before
			cells[key] = cell
		}
		id := info.StackNetworkID
		if id < 0 && id == req.RequestID && cell.predicted {
			return cell, true
		}
		if id < 0 {
			previous, exists := menu.receipts[id]
			if !exists {
				return nil, false
			}
			id, exists = previous.ids[key]
			if !exists {
				return nil, false
			}
		}
		// Within a request Bedrock refers to the initial slot network ID.
		return cell, id == chestItemID(cell.before)
	}
	if !planEditable(req, get) {
		s.rejectEditable(req.RequestID)
		return
	}
	for _, cell := range cells {
		if !menu.source.AllowChange(p, cell.menuSlot, cell.before, cell.after) {
			s.rejectEditable(req.RequestID)
			return
		}
		current, err := cell.inv.Item(cell.slot)
		if err != nil || chestItemID(current) != chestItemID(cell.before) || current.Count() != cell.before.Count() {
			s.rejectEditable(req.RequestID)
			return
		}
	}
	// Reserve enough personal inventory space to settle the cursor on close.
	// This prevents a full viewer inventory from stranding transferred items in
	// temporary UI slots (host drop handlers may refuse vanilla overflow drops).
	cursor, _ := s.ui.Item(0)
	personal := s.inv.Slots()
	for _, cell := range cells {
		if cell.inv == s.ui && cell.slot == 0 {
			cursor = cell.after
		}
		if cell.inv == s.inv {
			personal[cell.slot] = cell.after
		}
	}
	remaining := cursor.Count()
	for _, stack := range personal {
		if cursor.Empty() {
			break
		}
		if stack.Empty() {
			remaining -= cursor.MaxCount()
		} else if cursor.Comparable(stack) {
			remaining -= max(0, stack.MaxCount()-stack.Count())
		}
	}
	if remaining > 0 {
		s.rejectEditable(req.RequestID)
		return
	}
	// All authoritative inventories belong to p.Tx(); no other transaction may
	// interleave between this validation and commit. No inventory handlers are
	// bypassed implicitly: the source explicitly authorizes these server writes.
	keys := make([]editableKey, 0, len(cells))
	for key := range cells {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].container == keys[j].container {
			return keys[i].slot < keys[j].slot
		}
		return keys[i].container < keys[j].container
	})
	for _, key := range keys {
		cell := cells[key]
		if err := cell.inv.SetItem(cell.slot, cell.after); err != nil {
			for _, rollback := range cells {
				_ = rollback.inv.SetItem(rollback.slot, rollback.before)
			}
			s.rejectEditable(req.RequestID)
			return
		}
	}
	receipt := editableReceipt{at: now, ids: make(map[editableKey]int32)}
	containers := make(map[byte][]protocol.StackResponseSlotInfo)
	for _, key := range keys {
		stack := cells[key].after
		receipt.ids[key] = chestItemID(stack)
		containers[key.container] = append(containers[key.container], protocol.StackResponseSlotInfo{Slot: key.slot, HotbarSlot: key.slot, Count: byte(stack.Count()), StackNetworkID: chestItemID(stack), DurabilityCorrection: int32(stack.MaxDurability() - stack.Durability())})
	}
	menu.receipts[req.RequestID] = receipt
	response := protocol.ItemStackResponse{Status: protocol.ItemStackResponseStatusOK, RequestID: req.RequestID}
	for container, slots := range containers {
		response.ContainerInfo = append(response.ContainerInfo, protocol.StackResponseContainerInfo{Container: protocol.FullContainerName{ContainerID: container}, SlotInfo: slots})
	}
	session_writePacket(s.Session, &packet.ItemStackResponse{Responses: []protocol.ItemStackResponse{response}})
	s.refreshEditable(bindings)
}

func (s *chestSession) rejectEditable(id int32) {
	session_writePacket(s.Session, &packet.ItemStackResponse{Responses: []protocol.ItemStackResponse{{Status: protocol.ItemStackResponseStatusError, RequestID: id}}})
	session_sendInv(s.Session, s.inv, protocol.WindowIDInventory)
	session_sendInv(s.Session, s.ui, protocol.WindowIDUI)
	session_sendInv(s.Session, s.openedWindow.Load(), s.openedWindowID.Load())
}

func (s *chestSession) refreshEditable(bindings []EditableSlot) {
	stacks, ok := editableStacks(bindings)
	if !ok {
		return
	}
	for slot, stack := range stacks {
		_ = s.openedWindow.Load().SetItem(slot, stack)
	}
	session_sendInv(s.Session, s.openedWindow.Load(), s.openedWindowID.Load())
	session_sendInv(s.Session, s.inv, protocol.WindowIDInventory)
	session_sendInv(s.Session, s.ui, protocol.WindowIDUI)
}

// planEditable changes only private working values; rejection never writes authoritative slots.
func planEditable(req protocol.ItemStackRequest, get func(protocol.StackRequestSlotInfo) (*editableCell, bool)) bool {
	for _, action := range req.Actions {
		var from, to protocol.StackRequestSlotInfo
		var count byte
		swap := false
		switch a := action.(type) {
		case *protocol.TakeStackRequestAction:
			from, to, count = a.Source, a.Destination, a.Count
		case *protocol.PlaceStackRequestAction:
			from, to, count = a.Source, a.Destination, a.Count
		case *protocol.SwapStackRequestAction:
			from, to, swap = a.Source, a.Destination, true
		default:
			return false
		}
		source, okSource := get(from)
		destination, okDestination := get(to)
		if !okSource || !okDestination || source == destination {
			return false
		}
		if swap {
			source.after, destination.after = destination.after, source.after
			source.predicted, destination.predicted = true, true
			continue
		}
		if count == 0 || source.after.Empty() || int(count) > source.after.Count() || (!destination.after.Empty() && !source.after.Comparable(destination.after)) {
			return false
		}
		if destination.after.Count()+int(count) > source.after.MaxCount() {
			return false
		}
		if destination.after.Empty() {
			destination.after = source.after.Grow(int(count) - source.after.Count())
		} else {
			destination.after = destination.after.Grow(int(count))
		}
		source.after = source.after.Grow(-int(count))
		source.predicted, destination.predicted = true, true
	}
	return true
}
