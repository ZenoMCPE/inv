package inv

import (
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/player/form"
	"sync"
)

var formHandlers sync.Map

// HandleForm registers optional form presentation for a player. Use SendForm
// at the application's form call sites; upstream Player.SendForm is unchanged.
// The handler must not capture the transaction-scoped player.
func HandleForm(p *player.Player, handler func(*player.Player, form.Form) bool) {
	formHandlers.Store(player_session(p), handler)
}

// SendForm dispatches through the registered presentation handler, falling back
// to Dragonfly forms after closing any virtual chest.
func SendForm(submitter form.Submitter, f form.Form) {
	if p, ok := submitter.(*player.Player); ok {
		if handler, exists := formHandlers.Load(player_session(p)); exists && handler.(func(*player.Player, form.Form) bool)(p, f) {
			return
		}
		CloseChestMenu(p)
	}
	submitter.SendForm(f)
}

// Forget must be called from HandleQuit to release per-player state.
func Forget(p *player.Player) {
	CloseChestMenu(p)
	CloseContainer(p)
	formHandlers.Delete(player_session(p))
}
