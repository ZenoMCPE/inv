# Opt-in editable inventories

`OpenEditableMenu` adds a separate 27/54-slot live view. Existing constructors,
form dispatch, read-only chest menus and custom menus retain their behaviour.

An `EditableSource` resolves authoritative inventory slots inside each viewer
transaction. It must verify original viewer/target sessions, permissions and
same-world ownership on every resolution. It must never return an inventory
belonging to another active world. Invalid resolution closes the window.
Bindings are not retained. Only immutable stack receipts and the source's stable
handles/IDs survive transactions. Callbacks must not change inventory state.

Take/place/swap requests are planned on private values, checked for network IDs,
count conservation, slot aliasing, capacity and authorization, then committed in
the current world transaction. The source explicitly authorizes server writes;
normal inventory event handlers are not called. Its AllowChange must enforce
equipment validity, protected items and the host's edit policy. Refresh samples
the source every 100 ms and stops when the window/session is gone.

Drop, destroy, crafting and legacy InventoryTransaction edits fail closed.
They are not advertised as equivalent to every PocketMine InvMenu transaction.
CloseChestMenu/Forget remain mandatory on world transfers and disconnects.
The first release requires actual Bedrock checks of cursor/predicted requests,
split/merge/swap, concurrent changes, permissions, disconnect/reconnect and
world transfer, plus existing read-only menus and kit-editor regression checks.
