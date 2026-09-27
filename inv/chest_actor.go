package inv

import (
	"fmt"
	"slices"
	"sync"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/item/inventory"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/session"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// CompactChestSlots selects a real five-row inventory using InventoryUIResourcePack.
const CompactChestSlots = 45

const chestActorType = "inventoryui:inventoryui"

// The prefix is InventoryUIResourcePack's five-row, non-scrolling selector.
const compactChestTitlePrefix = "§5§0§r§r§r§r§r§r§r§r§r"

func (s *chestSession) openActorChestMenu(tx *world.Tx, c session.Controllable, title string, slots []item.Stack, keepOpen []int, submit func(session.Controllable, int, *world.Tx)) {
	if current := s.chestMenu; current != nil && current.actorID != 0 {
		inv := s.openedWindow.Load()
		for slot, stack := range slots {
			_ = inv.SetItem(slot, stack)
		}
		s.chestMenu = &chestMenu{title: title, actorID: current.actorID, keepOpen: slices.Clone(keepOpen), submit: submit}
		if current.title != title {
			session_writePacket(s.Session, &packet.SetActorData{EntityRuntimeID: current.actorID, EntityMetadata: protocol.EntityMetadata{
				protocol.EntityDataKeyName: compactChestTitlePrefix + title,
			}})
		}
		session_sendInv(s.Session, inv, s.openedWindowID.Load())
		return
	}
	s.CloseChestMenu(tx)
	chestSessions.Store(s.Session, s)
	CloseContainer(c.(*player.Player))
	session_closeCurrentContainer(s.Session, tx, false)
	c.MoveItemsToInventory()

	inv := inventory.New(CompactChestSlots, nil)
	for slot, stack := range slots {
		_ = inv.SetItem(slot, stack)
	}
	actorID := s.nextChestActorID()
	pos := cube.PosFromVec3(c.Position())
	windowID := session_nextWindowID(s.Session)
	s.containerOpened.Store(true)
	s.openedContainerID.Store(protocol.ContainerTypeContainer)
	s.openedWindow.Store(inv)
	s.openedPos.Store(&pos)
	s.chestMenu = &chestMenu{title: title, actorID: actorID, keepOpen: slices.Clone(keepOpen), submit: submit}

	metadata := protocol.NewEntityMetadata()
	metadata[protocol.EntityDataKeyName] = compactChestTitlePrefix + title
	metadata[protocol.EntityDataKeyContainerType] = byte(protocol.ContainerTypeContainer)
	metadata[protocol.EntityDataKeyContainerSize] = int32(CompactChestSlots)
	metadata[protocol.EntityDataKeyScale] = float32(0)
	metadata[protocol.EntityDataKeyWidth] = float32(0)
	metadata[protocol.EntityDataKeyHeight] = float32(0)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagInvisible)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagNoAI)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagSilent)
	position := c.Position()
	session_writePacket(s.Session, &packet.AddActor{
		EntityUniqueID: int64(actorID), EntityRuntimeID: actorID, EntityType: chestActorType,
		Position: mgl32.Vec3{float32(position[0]), float32(position[1]), float32(position[2])}, EntityMetadata: metadata,
	})
	// InventoryUI attaches its invisible inventory actor to the player, whose
	// runtime/unique ID is always 1 in their own Dragonfly session.
	session_writePacket(s.Session, &packet.SetActorLink{EntityLink: protocol.EntityLink{
		RiddenEntityUniqueID: 1, RiderEntityUniqueID: int64(actorID), Type: protocol.EntityLinkRider, Immediate: true, RiderInitiated: true,
	}})
	// Queue the open and contents immediately after spawning/linking the actor.
	session_writePacket(s.Session, &packet.ContainerOpen{
		WindowID: windowID, ContainerType: protocol.ContainerTypeContainer,
		ContainerPosition: protocol.BlockPos{int32(pos[0]), int32(pos[1]), int32(pos[2])}, ContainerEntityUniqueID: int64(actorID),
	})
	session_sendInv(s.Session, inv, uint32(windowID))
}

func (s *chestSession) nextChestActorID() uint64 {
	// Share Dragonfly's allocator to avoid collisions with visible world actors.
	mu := privateFieldPointer[sync.RWMutex](s.Session, "entityMutex")
	mu.Lock()
	defer mu.Unlock()
	next := privateFieldPointer[uint64](s.Session, "currentEntityRuntimeID")
	*next++
	return *next
}

func (s *chestSession) removeChestActor(actorID uint64) {
	session_writePacket(s.Session, &packet.SetActorLink{EntityLink: protocol.EntityLink{
		RiddenEntityUniqueID: 1, RiderEntityUniqueID: int64(actorID), Type: protocol.EntityLinkRemove, Immediate: true,
	}})
	session_writePacket(s.Session, &packet.RemoveActor{EntityUniqueID: int64(actorID)})
}

func registerChestActor(pk *packet.AvailableActorIdentifiers) error {
	var data struct {
		Identifiers []map[string]any `nbt:"idlist"`
	}
	if err := nbt.Unmarshal(pk.SerialisedEntityIdentifiers, &data); err != nil {
		return fmt.Errorf("decode actor identifiers: %w", err)
	}
	for _, identifier := range data.Identifiers {
		if identifier["id"] == chestActorType {
			return nil
		}
	}
	data.Identifiers = append(data.Identifiers, map[string]any{
		"id": chestActorType, "bid": "", "hasspawnegg": byte(0), "summonable": byte(0),
	})
	encoded, err := nbt.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode actor identifiers: %w", err)
	}
	pk.SerialisedEntityIdentifiers = encoded
	return nil
}
