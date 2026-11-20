package inv

import "testing"

func TestWithActorOnlyAppliesToContainersAnActorCanOpen(t *testing.T) {
	for _, test := range []struct {
		container Container
		actor     bool
	}{
		{ContainerHopper{}, true},
		{ContainerChest{}, true},
		{ContainerDropper{}, false},
	} {
		m := NewCustomMenu("menu", test.container, nil, nil).WithActor()
		if m.actor != test.actor {
			t.Errorf("%T: actor = %v, want %v", test.container, m.actor, test.actor)
		}
	}
}
