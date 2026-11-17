package tools

import (
	"testing"
)

func TestDenyTools(t *testing.T) {
	client := testClient(t)

	t.Run("leaves denied tools out", func(t *testing.T) {
		opts := toolOptions("all")
		opts.DenyTools = map[string]bool{"proxmox_delete_vm": true, "proxmox_get_version": true}
		names := toolNames(t, connect(t, client, opts))
		falsy(t, names["proxmox_delete_vm"], "proxmox_delete_vm")
		falsy(t, names["proxmox_get_version"], "proxmox_get_version")
		truthy(t, names["proxmox_delete_container"], "proxmox_delete_container")
		truthy(t, names["proxmox_list_vms"], "proxmox_list_vms")
	})

	t.Run("accepts any existing tool, even one not enabled", func(t *testing.T) {
		if err := CheckDenyTools(nil); err != nil {
			t.Fatal(err)
		}
		// The raw tool is opt-in and exec tools need exec, but their names are still valid.
		if err := CheckDenyTools(map[string]bool{"proxmox_api_request": true, "proxmox_run_guest_command": true, "proxmox_delete_vm": true}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refuses unknown names", func(t *testing.T) {
		err := CheckDenyTools(map[string]bool{"proxmox_delete_vm": true, "proxmox_delte_vm": true, "bogus": true})
		if err == nil {
			t.Fatal("unknown names accepted")
		}
		match(t, err.Error(), `^Unknown tool "bogus", "proxmox_delte_vm" in PROXMOX_DENY_TOOLS`)
	})
}
