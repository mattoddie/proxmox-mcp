package proxmoxtest

import (
	"github.com/mattoddie/proxmox-mcp/internal/js"
)

// Fixture data, as JSON so every request gets a fresh copy.
const (
	NodesJSON = `[
  {"node":"pve1","status":"online","type":"node","id":"node/pve1","cpu":0.12,"maxcpu":16,"mem":17179869184,"maxmem":68719476736,"uptime":864000,"level":"","ssl_fingerprint":"AA:BB"},
  {"node":"pve2","status":"online","type":"node","id":"node/pve2","cpu":0.05,"maxcpu":8,"mem":8589934592,"maxmem":34359738368,"uptime":432000,"level":""}
]`
	GuestsJSON = `[
  {"id":"qemu/100","vmid":100,"type":"qemu","node":"pve1","name":"web","status":"running","cpu":0.031,"maxcpu":4,"mem":2147483648,"maxmem":4294967296,"disk":0,"maxdisk":34359738368,"uptime":3600,"netin":1000,"netout":2000,"tags":"prod;web","template":0,"pool":"production"},
  {"id":"qemu/101","vmid":101,"type":"qemu","node":"pve2","name":"db","status":"stopped","cpu":0,"maxcpu":2,"mem":0,"maxmem":8589934592,"disk":0,"maxdisk":107374182400,"uptime":0,"template":0},
  {"id":"lxc/200","vmid":200,"type":"lxc","node":"pve1","name":"dns","status":"running","cpu":0.002,"maxcpu":1,"mem":134217728,"maxmem":536870912,"disk":1073741824,"maxdisk":8589934592,"uptime":7200,"tags":"infra","template":0},
  {"id":"qemu/9000","vmid":9000,"type":"qemu","node":"pve1","name":"tpl-debian","status":"stopped","maxcpu":2,"maxmem":2147483648,"maxdisk":3221225472,"template":1}
]`
	StoragesJSON = `[
  {"id":"storage/pve1/local","storage":"local","type":"storage","node":"pve1","status":"available","plugintype":"dir","content":"iso,vztmpl,backup","disk":10737418240,"maxdisk":107374182400,"shared":0},
  {"id":"storage/pve1/local-lvm","storage":"local-lvm","type":"storage","node":"pve1","status":"available","plugintype":"lvmthin","content":"images,rootdir","disk":53687091200,"maxdisk":429496729600,"shared":0},
  {"id":"storage/pve2/local","storage":"local","type":"storage","node":"pve2","status":"available","plugintype":"dir","content":"iso,vztmpl,backup","disk":5368709120,"maxdisk":107374182400,"shared":0}
]`
	VM100ConfigJSON = `{"name":"web","cores":4,"sockets":1,"memory":"4096","ostype":"l26","boot":"order=scsi0;net0","scsihw":"virtio-scsi-single",
  "scsi0":"local-lvm:vm-100-disk-0,iothread=1,size=32G","ide2":"local:iso/debian-12.iso,media=cdrom","net0":"virtio=BC:24:11:00:00:01,bridge=vmbr0,firewall=1,tag=10",
  "agent":"1","cipassword":"hunter2","tags":"prod;web","digest":"abc123"}`
	CT200ConfigJSON = `{"hostname":"dns","arch":"amd64","cores":1,"memory":512,"swap":512,"ostype":"debian","rootfs":"local-lvm:vm-200-disk-0,size=8G",
  "net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:02,ip=dhcp,type=veth","unprivileged":1,"features":"nesting=1","tags":"infra","digest":"def456"}`
	clusterStatusJSON = `[
  {"type":"cluster","id":"cluster","name":"homelab","nodes":2,"quorate":1,"version":4},
  {"type":"node","id":"node/pve1","name":"pve1","nodeid":1,"online":1,"local":1,"ip":"10.0.0.11","level":""},
  {"type":"node","id":"node/pve2","name":"pve2","nodeid":2,"online":1,"local":0,"ip":"10.0.0.12","level":""}
]`
)

func resources() []any {
	var out []any
	for _, s := range []string{NodesJSON, GuestsJSON, StoragesJSON} {
		out = append(out, js.MustParse(s).([]any)...)
	}
	return append(out, js.O("id", "pool/production", "type", "pool", "pool", "production"))
}

var fixtures = map[string]func(params *js.Obj) any{
	"GET /version": func(*js.Obj) any { return js.O("version", "9.0.10", "release", "9.0", "repoid", "deadbeef") },
	"GET /nodes":   func(*js.Obj) any { return js.MustParse(NodesJSON) },
	"GET /cluster/resources": func(p *js.Obj) any {
		all := resources()
		t := p.Get("type")
		if !js.Truthy(t) {
			return all
		}
		out := []any{}
		for _, r := range js.Objs(all) {
			rt := r.Get("type")
			if (t == "vm" && (rt == "qemu" || rt == "lxc")) || rt == t {
				out = append(out, r)
			}
		}
		return out
	},
	"GET /cluster/status":             func(*js.Obj) any { return js.MustParse(clusterStatusJSON) },
	"GET /nodes/pve1/qemu/100/config": func(*js.Obj) any { return js.MustParse(VM100ConfigJSON) },
	"GET /nodes/pve1/lxc/200/config":  func(*js.Obj) any { return js.MustParse(CT200ConfigJSON) },
}
