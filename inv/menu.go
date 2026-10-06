package inv

import (
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	_ "unsafe"

	"github.com/df-mc/dragonfly/server/world"

	"github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// Menu is a menu that can be sent to a player. It can be used to display a custom inventory to a player.
type Menu struct {
	name      string
	container Container

	inventory      *inventory.Inventory
	containerClose func(inv *inventory.Inventory)

	submittable Submittable

	pos cube.Pos

	windowID byte
	custom   bool
	openTask *world.Task
}

// NewMenu creates a new menu with the submittable passed, the name passed and the container passed.
func NewMenu(submittable Submittable, name string, container Container) Menu {
	return Menu{name: name, submittable: submittable, container: container, inventory: inventory.New(container.Size(), func(slot int, before, after item.Stack) {})}
}

// NewCustomMenu creates a new menu with the name, container and inventory.
func NewCustomMenu(name string, container Container, inv *inventory.Inventory, containerClose func(inv *inventory.Inventory)) Menu {
	return Menu{name: name, container: container, inventory: inv, containerClose: containerClose, custom: true}
}

// WithStacks sets the stacks of the menu to the stacks passed.
func (m Menu) WithStacks(stacks ...item.Stack) Menu {
	m.inventory.Clear()
	for i, it := range stacks {
		_ = m.inventory.SetItem(i, it)
	}
	return m
}

// Submittable is a type that can be implemented by a Menu to be called when a menu is submitted.
type Submittable interface {
	Submit(p *player.Player, it item.Stack)
}

// Closer is a type that can be implemented by a Submittable to be called when a menu is closed.
type Closer interface {
	Close(p *player.Player)
}

// SendMenu sends a menu to a player, refreshing a compatible open window in place.
func SendMenu(p *player.Player, m Menu) {
	sendMenu(p, m, false)
}

// UpdateMenu updates the menu that the player passed is currently viewing to the menu passed.
func UpdateMenu(p *player.Player, m Menu) {
	sendMenu(p, m, true)
}

// sendMenu sends the menu to a player.
func sendMenu(p *player.Player, m Menu, update bool) {
	s := player_session(p)
	if s == session.Nop {
		return
	}
	if !m.custom {
		m.inventory.Handle(handler{menu: m})
	}

	pos := cube.PosFromVec3(p.Rotation().Vec3().Mul(-2).Add(p.Position())).Add(cube.Pos{0, 2, 0})
	var nextID byte
	current, exists := lastMenu(s)
	refresh := exists && menuActive(s, current) && current.container.Type() == m.container.Type() && current.container.Size() == m.container.Size() && (update || !current.custom && !m.custom)
	if refresh {
		pos, nextID, m.openTask = current.pos, current.windowID, current.openTask
	} else {
		closeMenus(p)
		session_closeCurrentContainer(s, p.Tx(), false)
		p.MoveItemsToInventory()
		nextID = session_nextWindowID(s)
	}
	blockPos := blockPosToProtocol(pos)

	s.ViewBlockUpdate(pos, m.container.Block(), 0)
	s.ViewBlockUpdate(pos.Add(cube.Pos{0, 1}), block.Air{}, 0)

	data := createFakeInventoryNBT(m.name, m.container)

	if m.container.Size() == 54 {
		s.ViewBlockUpdate(pos.Add(cube.Pos{1, 0, 0}), m.container.Block(), 0)
		s.ViewBlockUpdate(pos.Add(cube.Pos{1, 1}), block.Air{}, 0)

		data["pairz"] = int32(pos[2])
		data["pairx"] = int32(pos[0] + 1)
	}

	session_writePacket(s, &packet.BlockActorData{
		Position: blockPos,
		NBTData:  data,
	})

	privateFieldPointer[atomic.Pointer[cube.Pos]](s, "openedPos").Store(&pos)
	privateFieldPointer[atomic.Pointer[inventory.Inventory]](s, "openedWindow").Store(m.inventory)
	privateFieldPointer[atomic.Bool](s, "containerOpened").Store(true)
	privateFieldPointer[atomic.Uint32](s, "openedContainerID").Store(uint32(m.container.Type()))

	if !refresh {
		var task *world.Task
		task = p.H().DoAfter(time.Millisecond*500, func(_ *world.Tx, e world.Entity) {
			if player_session(e.(*player.Player)) != s {
				return
			}
			current, ok := lastMenu(s)
			if !ok || current.openTask != task || !menuActive(s, current) {
				return
			}
			current.openTask = nil
			menuMu.Lock()
			lastMenus[s] = current
			menuMu.Unlock()
			session_writePacket(s, &packet.ContainerOpen{
				WindowID:                current.windowID,
				ContainerPosition:       blockPosToProtocol(current.pos),
				ContainerType:           byte(current.container.Type()),
				ContainerEntityUniqueID: -1,
			})
			session_sendInv(s, current.inventory, uint32(current.windowID))
		})
		m.openTask = task
	} else if m.openTask == nil {
		session_sendInv(s, m.inventory, uint32(nextID))
	}

	m.pos = pos
	m.windowID = nextID

	menuMu.Lock()
	lastMenus[s] = m
	menuMu.Unlock()
}

var (
	menuMu    sync.Mutex
	lastMenus = map[*session.Session]Menu{}
)

func lastMenu(s *session.Session) (Menu, bool) {
	menuMu.Lock()
	defer menuMu.Unlock()
	m, ok := lastMenus[s]
	return m, ok
}

func menuActive(s *session.Session, m Menu) bool {
	return privateFieldPointer[atomic.Bool](s, "containerOpened").Load() &&
		privateFieldPointer[atomic.Pointer[inventory.Inventory]](s, "openedWindow").Load() == m.inventory &&
		byte(privateFieldPointer[atomic.Uint32](s, "openedWindowID").Load()) == m.windowID
}

func closeLastMenu(p *player.Player, mn Menu, clientRequested bool) {
	removeLastMenu(p, mn, clientRequested)
	if player_session(p) == session.Nop {
		return
	}
	if closeable, ok := mn.submittable.(Closer); ok {
		closeable.Close(p)
	}
	if mn.containerClose != nil {
		mn.containerClose(mn.inventory)
	}
}

func removeLastMenu(p *player.Player, mn Menu, clientRequested bool) {
	s := player_session(p)
	menuMu.Lock()
	delete(lastMenus, s)
	menuMu.Unlock()
	if mn.openTask != nil {
		mn.openTask.Cancel()
	}
	if s != session.Nop {
		if menuActive(s, mn) {
			session_closeWindow(s, clientRequested)
		}
		removeClientSideMenu(s, p.Tx(), mn)
	}
}

func removeClientSideMenu(s *session.Session, tx *world.Tx, m Menu) {
	s.ViewBlockUpdate(m.pos, tx.Block(m.pos), 0)
	airPos := m.pos.Add(cube.Pos{0, 1})
	s.ViewBlockUpdate(airPos, tx.Block(airPos), 0)
	if c, ok := m.container.(ContainerChest); ok && c.DoubleChest {
		pairPos := m.pos.Add(cube.Pos{1, 0, 0})
		s.ViewBlockUpdate(pairPos, tx.Block(pairPos), 0)
		airPos = m.pos.Add(cube.Pos{1, 1})
		s.ViewBlockUpdate(airPos, tx.Block(airPos), 0)
	}
}

// blockPosToProtocol converts a cube.Pos to a protocol.BlockPos.
func blockPosToProtocol(pos cube.Pos) protocol.BlockPos {
	return protocol.BlockPos{int32(pos[0]), int32(pos[1]), int32(pos[2])}
}

// createFakeInventoryNBT creates a map of NBT data for a fake inventory with the name passed and the inventory
func createFakeInventoryNBT(name string, container Container) map[string]interface{} {
	m := map[string]interface{}{"CustomName": name}
	switch container.Type() {
	case protocol.ContainerTypeContainer:
		m["id"] = "Chest"
	case protocol.ContainerTypeHopper:
		m["id"] = "Hopper"
	case protocol.ContainerTypeDropper:
		m["id"] = "Dropper"
	default:
		panic("should never happen")
	}
	return m
}

// fetchPrivateField fetches a private field of a session.
func fetchPrivateField[T any](s *session.Session, name string) T {
	reflectedValue := reflect.ValueOf(s).Elem()
	privateFieldValue := reflectedValue.FieldByName(name)
	privateFieldValue = reflect.NewAt(privateFieldValue.Type(), unsafe.Pointer(privateFieldValue.UnsafeAddr())).Elem()

	return privateFieldValue.Interface().(T)
}

// noinspection ALL
//
//go:linkname player_session github.com/df-mc/dragonfly/server/player.(*Player).session
func player_session(*player.Player) *session.Session

// noinspection ALL
//
//go:linkname session_writePacket github.com/df-mc/dragonfly/server/session.(*Session).writePacket
func session_writePacket(*session.Session, packet.Packet)

// noinspection ALL
//
//go:linkname session_nextWindowID github.com/df-mc/dragonfly/server/session.(*Session).nextWindowID
func session_nextWindowID(*session.Session) byte

// noinspection ALL
//
//go:linkname session_sendInv github.com/df-mc/dragonfly/server/session.(*Session).sendInv
func session_sendInv(*session.Session, *inventory.Inventory, uint32)
