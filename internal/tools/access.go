package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

const accessToolset = "access"

// ---------------------------------------------------------------- arguments

var (
	accessUseridArg  = Str().D("User ID in name@realm form, e.g. alice@pve or root@pam (see proxmox_list_users)")
	accessGroupidArg = Str().D("Group ID (see proxmox_list_groups)")
	accessRoleidArg  = Str().D("Role ID, e.g. PVEVMAdmin (see proxmox_list_roles)")
	accessRealmArg   = Str().D("Realm (authentication domain) ID, e.g. pam, pve or a configured LDAP/AD/OpenID realm (see proxmox_list_realms)")
	accessTokenidArg = Str().D("Token name (the part after '!', e.g. 'automation' for alice@pve!automation)")
	accessExpireArg  = Union(Int().Min(0), Str()).Opt().D("Expiry as UNIX seconds or an ISO date/time (e.g. 2027-01-31 or 2027-01-31T12:00:00Z); 0 = never expires")
)

// ---------------------------------------------------------------- helpers

var (
	accessDigitsRe = regexp.MustCompile(`^\d+$`)
	// accessISORe is the ISO 8601 subset Date.parse accepts: date-only forms are UTC, date-times
	// without an offset are local time.
	accessISORe = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(Z|[+-]\d{2}:?\d{2})?)?$`)
	// accessLegacyLayouts approximate the non-ISO forms V8's Date.parse falls back to (local time
	// unless a zone is given).
	accessLegacyLayouts = []string{
		"2006-1-2", "2006-1-2 15:04", "2006-1-2 15:04:05", "2006-1-2T15:04", "2006-1-2T15:04:05",
		"2006/1/2", "2006/1/2 15:04", "2006/1/2 15:04:05",
		"Jan 2 2006", "Jan 2, 2006", "January 2 2006", "January 2, 2006", "2 Jan 2006", "2 January 2006",
		"Jan 2 2006 15:04:05", "Jan 2, 2006 15:04:05",
		time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC, time.UnixDate, time.RFC822, time.RFC822Z,
		"Mon Jan 2 2006", "Mon Jan 02 2006 15:04:05 GMT-0700",
	}
)

// accessDateParse is Date.parse: epoch milliseconds, or NaN.
func accessDateParse(s string) float64 {
	if m := accessISORe.FindStringSubmatch(s); m != nil {
		num := func(x string, def int) int {
			if x == "" {
				return def
			}
			n, _ := strconv.Atoi(x)
			return n
		}
		year := num(m[1], 0)
		month, day := num(m[2], 1), num(m[3], 1)
		hour, minute, sec := num(m[4], 0), num(m[5], 0), num(m[6], 0)
		ms := 0
		if m[7] != "" {
			frac := (m[7] + "00")[:3]
			ms, _ = strconv.Atoi(frac)
		}
		if month < 1 || month > 12 || day < 1 || day > 31 || hour > 24 || minute > 59 || sec > 59 || (hour == 24 && (minute != 0 || sec != 0 || ms != 0)) {
			return math.NaN()
		}
		loc := time.UTC
		switch {
		case m[8] == "" && m[4] != "":
			loc = time.Local
		case m[8] != "" && m[8] != "Z":
			off := strings.ReplaceAll(m[8][1:], ":", "")
			h, _ := strconv.Atoi(off[:2])
			mi, _ := strconv.Atoi(off[2:])
			secs := h*3600 + mi*60
			if m[8][0] == '-' {
				secs = -secs
			}
			loc = time.FixedZone("", secs)
		}
		t := time.Date(year, time.Month(month), day, hour, minute, sec, ms*int(time.Millisecond), loc)
		return float64(t.UnixMilli())
	}
	trimmed := strings.TrimSpace(s)
	for _, layout := range accessLegacyLayouts {
		if t, err := time.ParseInLocation(layout, trimmed, time.Local); err == nil {
			return float64(t.UnixMilli())
		}
	}
	return math.NaN()
}

// accessToEpoch turns epoch numbers or ISO date strings into UNIX seconds (undefined stays undefined).
func accessToEpoch(value any) (any, error) {
	switch v := value.(type) {
	case js.Undefined:
		return js.Undef, nil
	case float64:
		return v, nil
	case string:
		t := strings.TrimSpace(v)
		if accessDigitsRe.MatchString(t) {
			return js.Num(t), nil
		}
		ms := accessDateParse(v)
		if !js.IsFinite(ms) {
			return nil, fmt.Errorf(`Invalid date/time "%s"; use UNIX seconds or ISO 8601 such as 2027-01-31T12:00:00Z`, v)
		}
		return math.Floor(ms / 1000), nil
	}
	return value, nil
}

// accessExpiry is "never" for 0, otherwise ISO; undefined when absent.
func accessExpiry(value any) any {
	if js.Nullish(value) || value == "" {
		return js.Undef
	}
	if js.Num(value) == 0 {
		return "never"
	}
	return proxmox.IsoTime(value)
}

var accessListSplitRe = regexp.MustCompile(`[,;\s]+`)

func accessSplitList(value any) any {
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, v := range arr {
			out[i] = js.Str(v)
		}
		return out
	}
	s, ok := value.(string)
	if !ok || s == "" {
		return js.Undef
	}
	out := []any{}
	for _, p := range accessListSplitRe.Split(s, -1) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// accessJoinList joins a list with commas (undefined stays undefined).
func accessJoinList(list any) any {
	if arr, ok := list.([]any); ok {
		return js.Str(arr)
	}
	return js.Undef
}

func accessRealmOf(userid string) any {
	i := strings.LastIndex(userid, "@")
	if i < 0 {
		return js.Undef
	}
	return strings.Split(userid[i+1:], "!")[0]
}

func accessNowSeconds() float64 { return float64(time.Now().UnixMilli()) / 1000 }

func accessSummarizeUser(u *Obj) *Obj {
	lockedUntil := u.Num("tfa-locked-until")
	tokensArr, isArr := u.Get("tokens").([]any)
	var names []string
	for _, k := range []string{"firstname", "lastname"} {
		if v := u.Get(k); js.Truthy(v) {
			names = append(names, js.Str(v))
		}
	}
	var tokens, tokenIDs any = js.Undef, js.Undef
	if isArr {
		tokens = len(tokensArr)
		tokenIDs = mapObjs(js.Objs(tokensArr), func(t *Obj) any { return t.Get("tokenid") })
	}
	return proxmox.C(
		"userid", u.Get("userid"),
		"enabled", or(proxmox.Flag(u.Get("enable")), true),
		"expire", accessExpiry(u.Get("expire")),
		"realm", accessRealmOf(js.Str(or(u.Get("userid"), ""))),
		"realm_type", u.Get("realm-type"),
		"name", orUndef(strings.Join(names, " ")),
		"email", u.Get("email"),
		"groups", accessSplitList(u.Get("groups")),
		"tokens", tokens,
		"token_ids", tokenIDs,
		"totp_locked", orUndef(proxmox.Flag(u.Get("totp-locked"))),
		"tfa_locked_until", ifThen(lockedUntil > accessNowSeconds(), proxmox.IsoTime(lockedUntil)),
		"comment", u.Get("comment"),
	)
}

func accessSummarizeToken(userid, tokenid any, t *Obj) *Obj {
	return proxmox.C(
		"id", js.Str(userid)+"!"+js.Str(tokenid),
		"userid", userid,
		"tokenid", tokenid,
		"privsep", or(proxmox.Flag(t.Get("privsep")), true),
		"expire", or(accessExpiry(t.Get("expire")), "same as user"),
		"comment", t.Get("comment"),
	)
}

// accessRolePrivs normalises a role's privileges, which come as a comma list (index) or a
// {priv: 1} map (get).
func accessRolePrivs(r any) []string {
	if !js.Truthy(r) {
		return []string{}
	}
	if s, ok := r.(string); ok {
		out := []string{}
		if l, ok := accessSplitList(s).([]any); ok {
			for _, x := range l {
				out = append(out, js.Str(x))
			}
		}
		return out
	}
	out := []string{}
	if o, ok := r.(*Obj); ok {
		for _, k := range o.Keys() {
			if js.Truthy(proxmox.Flag(o.Get(k))) {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// accessGroupPrivs groups privileges by prefix: {VM: ["VM.Audit", ...], Sys: [...]}.
func accessGroupPrivs(privs []string) *Obj {
	sorted := append([]string(nil), privs...)
	sort.Strings(sorted)
	out := js.NewObj()
	for _, p := range sorted {
		area := strings.Split(p, ".")[0]
		list, _ := out.Get(area).([]any)
		out.Set(area, append(list, p))
	}
	return out
}

var (
	accessRealmSecretKeys = []string{"password", "client-key", "bind_password"}
	accessTfaKeyRe        = regexp.MustCompile(`(^|,)key=[^,]*`)
)

// accessRedactTfa redacts a Yubico API key in a realm `tfa` property string (type=yubico,id=...,key=...).
func accessRedactTfa(tfa any) any {
	if s, ok := tfa.(string); ok {
		return accessTfaKeyRe.ReplaceAllString(s, "${1}key=<redacted>")
	}
	return tfa
}

func accessRedactRealm(r any) any {
	out := proxmox.RedactSecrets(r)
	o, ok := out.(*Obj)
	if !ok {
		return out
	}
	for _, k := range accessRealmSecretKeys {
		if v := o.Get(k); !js.IsUndef(v) && v != "" {
			o.Set(k, "<redacted>")
		}
	}
	if o.Defined("tfa") {
		o.Set("tfa", accessRedactTfa(o.Get("tfa")))
	}
	return o
}

func accessSummarizeTfaEntry(e *Obj) any {
	return proxmox.C("id", e.Get("id"), "type", e.Get("type"), "description", e.Get("description"), "created", proxmox.IsoTime(e.Get("created")), "enabled", or(proxmox.Flag(e.Get("enable")), true))
}

func accessCreatedKey(a *Obj) string { return pick(a.Truthy("create"), "created", "updated").(string) }

func accessArrLen(a *Obj, k string) int { return len(a.Arr(k)) }

func accessAsObj(v any) *Obj {
	o, _ := v.(*Obj)
	return o
}

func registerAccess(c *Context) {
	client := c.Client

	// ================================================================ users

	c.Define("proxmox_list_users", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List users",
		Description: "List Proxmox users with enabled state, expiry, realm, name, email, groups, API token count and TFA lock-out state. Use proxmox_get_user for one user's details and tokens.",
		Input: []Prop{
			P("enabled", Bool().Opt().D("Only enabled (true) or disabled (false) users")),
			P("realm", Str().Opt().D("Only users of this realm, e.g. pve or pam")),
			P("group", Str().Opt().D("Only members of this group")),
			P("search", searchArg),
			P("limit", limitArg(500)),
			P("raw", rawArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			users, err := client.GetList(ctx, "/access/users", defined(js.O("full", 1, "enabled", a.Get("enabled"))))
			if err != nil {
				return nil, err
			}
			if a.Truthy("realm") {
				users = filterObjs(users, func(u *Obj) bool { return accessRealmOf(js.Str(u.Get("userid"))) == a.Get("realm") })
			}
			if a.Truthy("group") {
				users = filterObjs(users, func(u *Obj) bool {
					l, _ := accessSplitList(u.Get("groups")).([]any)
					for _, g := range l {
						if g == a.Get("group") {
							return true
						}
					}
					return false
				})
			}
			sortObjs(users, func(x, y *Obj) float64 { return compareStr(x.Get("userid"), y.Get("userid")) })
			res := filterAndLimit(users, a.Get("search"), or(a.Get("limit"), 500), func(u *Obj) []any {
				return []any{u.Get("userid"), u.Get("firstname"), u.Get("lastname"), u.Get("email"), u.Get("comment"), u.Get("groups")}
			})
			if !a.Truthy("raw") {
				return res.Map(func(u *Obj) any { return accessSummarizeUser(u) }), nil
			}
			return res.Map(func(u *Obj) any {
				return proxmox.RedactSecrets(js.Spread(u, js.O("keys", ifThen(u.Truthy("keys"), "<redacted>"))))
			}), nil
		},
	})

	c.Define("proxmox_get_user", Def{
		Toolset:     accessToolset,
		Title:       "Get user",
		Description: "Get one user's configuration: enabled state, expiry, name, email, groups, comment, API tokens and TFA entries. For effective permissions use proxmox_get_permissions.",
		Input:       []Prop{P("userid", accessUseridArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			u, err := client.GetObj(ctx, "/access/users/"+enc(a.Get("userid")), nil)
			if err != nil {
				return nil, err
			}
			var tfa any = js.Undef
			if list, err := client.GetList(ctx, "/access/tfa/"+enc(a.Get("userid")), nil); err == nil {
				tfa = mapObjs(list, accessSummarizeTfaEntry)
			}
			tokens := []any{}
			switch t := u.Get("tokens").(type) {
			case *Obj:
				for _, id := range t.Keys() {
					tokens = append(tokens, accessSummarizeToken(a.Get("userid"), id, accessObjOrEmpty(t.Get(id))))
				}
			case []any:
				for i, x := range t {
					tokens = append(tokens, accessSummarizeToken(a.Get("userid"), strconv.Itoa(i), accessObjOrEmpty(x)))
				}
			}
			return proxmox.Compact(js.Spread(
				accessSummarizeUser(js.Spread(u, js.O("userid", a.Get("userid"), "tokens", js.Undef))),
				js.O(
					"firstname", u.Get("firstname"),
					"lastname", u.Get("lastname"),
					"yubico_keys", ifThen(u.Truthy("keys"), "<set>"),
					"tokens", tokens,
					"tfa", tfa,
				),
			)), nil
		},
	})

	c.Define("proxmox_save_user", Def{
		Toolset:     accessToolset,
		Title:       "Create or update a user",
		Description: "Create (create=true) or update a Proxmox user. Only the given fields change on update. `password` is only accepted on create and only for the pve realm (pam users need a Linux account on every node); change existing passwords with proxmox_change_password. Groups replace the user's groups unless append=true. Grant permissions afterwards with proxmox_update_acl.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("userid", accessUseridArg),
			P("create", Bool().Opt().D("Create a new user (default false = update an existing one)")),
			P("password", Str().Opt().D("Initial password (create only, pve realm; at least 8 characters). Never echoed back.")),
			P("email", Str().Opt().D("Email address")),
			P("firstname", Str().Opt().D("First name")),
			P("lastname", Str().Opt().D("Last name")),
			P("groups", Arr(Str()).Opt().D("Group IDs the user belongs to (replaces the list unless append=true)")),
			P("append", Bool().Opt().D("On update, add `groups` to the existing groups instead of replacing them")),
			P("expire", accessExpireArg),
			P("enable", Bool().Opt().D("Enable (true) or disable (false) the account")),
			P("comment", Str().Opt().D("Comment")),
			P("keys", Str().Opt().D("Yubico OTP key IDs for two-factor auth (legacy)")),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Defined("password") && !a.Truthy("create") {
				return nil, errors.New("password can only be set when creating a user; use proxmox_change_password for existing users")
			}
			expire, err := accessToEpoch(a.Get("expire"))
			if err != nil {
				return nil, err
			}
			params, err := definedExtra(js.O(
				"email", a.Get("email"),
				"firstname", a.Get("firstname"),
				"lastname", a.Get("lastname"),
				"groups", accessJoinList(a.Get("groups")),
				"expire", expire,
				"enable", a.Get("enable"),
				"comment", a.Get("comment"),
				"keys", a.Get("keys"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			userPath := "/access/users/" + enc(a.Get("userid"))
			if a.Truthy("create") {
				_, err = client.Post(ctx, "/access/users", js.Spread(js.O("userid", a.Get("userid")), defined(js.O("password", a.Get("password"))), params))
			} else {
				_, err = client.Put(ctx, userPath, js.Spread(defined(js.O("append", a.Get("append"))), params))
			}
			if err != nil {
				return nil, err
			}
			u, err := client.GetObj(ctx, userPath, nil)
			if err != nil {
				return nil, err
			}
			return js.O(accessCreatedKey(a), a.Get("userid"), "user", accessSummarizeUser(js.Spread(u, js.O("userid", a.Get("userid"), "tokens", js.Undef)))), nil
		},
	})

	c.Define("proxmox_delete_user", Def{
		Toolset:     accessToolset,
		Title:       "Delete a user",
		Description: "Delete a user, together with their API tokens and ACL entries. To only block logins, disable the user with proxmox_save_user enable=false.",
		Delete:      true,
		Input:       []Prop{P("userid", accessUseridArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/users/"+enc(a.Get("userid")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("userid")), nil
		},
	})

	c.Define("proxmox_change_password", Def{
		Toolset:     accessToolset,
		Title:       "Change a user's password",
		Description: "Set a new password for a pve- or pam-realm user (PUT /access/password). The password is never echoed back. Changing another user's password needs User.Modify/Realm.AllocateUser; with ticket auth Proxmox may also require the caller's current password as confirmation_password.",
		Write:       true,
		Destructive: True,
		Idempotent:  true,
		Input: []Prop{
			P("userid", accessUseridArg),
			P("password", Str().Min(5).D("The new password (pve realm requires at least 8 characters)")),
			P("confirmation_password", Str().Opt().D("Current password of the user making the change, if Proxmox asks for it")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Put(ctx, "/access/password", defined(js.O("userid", a.Get("userid"), "password", a.Get("password"), "confirmation-password", a.Get("confirmation_password")))); err != nil {
				return nil, err
			}
			return js.O("password_changed", a.Get("userid")), nil
		},
	})

	// ================================================================ groups

	c.Define("proxmox_list_groups", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List groups",
		Description: "List user groups with their members and comments.",
		Input:       []Prop{P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			groups, err := client.GetList(ctx, "/access/groups", nil)
			if err != nil {
				return nil, err
			}
			sortObjs(groups, func(x, y *Obj) float64 { return compareStr(x.Get("groupid"), y.Get("groupid")) })
			res := filterAndLimit(groups, a.Get("search"), js.Undef, func(g *Obj) []any { return []any{g.Get("groupid"), g.Get("comment"), g.Get("users")} })
			return res.Map(func(g *Obj) any {
				return proxmox.C("groupid", g.Get("groupid"), "members", or(accessSplitList(g.Get("users")), []any{}), "comment", g.Get("comment"))
			}), nil
		},
	})

	c.Define("proxmox_get_group", Def{
		Toolset:     accessToolset,
		Title:       "Get group",
		Description: "Get a group's members and comment.",
		Input:       []Prop{P("groupid", accessGroupidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			g, err := client.GetObj(ctx, "/access/groups/"+enc(a.Get("groupid")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.C("groupid", a.Get("groupid"), "members", or(g.Get("members"), []any{}), "comment", g.Get("comment")), nil
		},
	})

	c.Define("proxmox_save_group", Def{
		Toolset:     accessToolset,
		Title:       "Create or update a group",
		Description: "Create (create=true) a user group or update its comment. Membership is managed on the user side: proxmox_save_user with groups (and append=true to add).",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("groupid", accessGroupidArg),
			P("create", Bool().Opt().D("Create a new group (default false = update an existing one)")),
			P("comment", Str().Opt().D("Comment")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var err error
			if a.Truthy("create") {
				_, err = client.Post(ctx, "/access/groups", defined(js.O("groupid", a.Get("groupid"), "comment", a.Get("comment"))))
			} else {
				_, err = client.Put(ctx, "/access/groups/"+enc(a.Get("groupid")), defined(js.O("comment", a.Get("comment"))))
			}
			if err != nil {
				return nil, err
			}
			return js.O(accessCreatedKey(a), a.Get("groupid")), nil
		},
	})

	c.Define("proxmox_delete_group", Def{
		Toolset:     accessToolset,
		Title:       "Delete a group",
		Description: "Delete a user group (users stay; their membership and the group's ACL entries are removed).",
		Delete:      true,
		Input:       []Prop{P("groupid", accessGroupidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/groups/"+enc(a.Get("groupid")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("groupid")), nil
		},
	})

	// ================================================================ roles and privileges

	c.Define("proxmox_list_roles", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List roles",
		Description: "List roles with their privileges. Built-in roles (Administrator, PVEAdmin, PVEVMAdmin, PVEAuditor, NoAccess...) are flagged builtin=true and cannot be changed. Use privileges=false for names only.",
		Input: []Prop{
			P("privileges", Bool().Opt().D("Include each role's privilege list (default true)")),
			P("builtin", Bool().Opt().D("Only built-in (true) or only custom (false) roles")),
			P("search", searchArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			roles, err := client.GetList(ctx, "/access/roles", nil)
			if err != nil {
				return nil, err
			}
			if a.Defined("builtin") {
				roles = filterObjs(roles, func(r *Obj) bool { return js.Truthy(proxmox.Flag(r.Get("special"))) == a.Bool("builtin") })
			}
			sortObjs(roles, func(x, y *Obj) float64 { return compareStr(x.Get("roleid"), y.Get("roleid")) })
			res := filterAndLimit(roles, a.Get("search"), js.Undef, func(r *Obj) []any { return []any{r.Get("roleid"), r.Get("privs")} })
			return res.Map(func(r *Obj) any {
				privs := accessRolePrivs(r.Get("privs"))
				return proxmox.C(
					"roleid", r.Get("roleid"),
					"builtin", orUndef(proxmox.Flag(r.Get("special"))),
					"privilege_count", len(privs),
					"privileges", ifThen(a.Get("privileges") != false, js.Array(privs)),
				)
			}), nil
		},
	})

	c.Define("proxmox_get_role", Def{
		Toolset:     accessToolset,
		Title:       "Get role",
		Description: "Get the privileges of one role, grouped by area (VM, Datastore, Sys...).",
		Input:       []Prop{P("roleid", accessRoleidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			r, err := client.Get(ctx, "/access/roles/"+enc(a.Get("roleid")), nil)
			if err != nil {
				return nil, err
			}
			privs := accessRolePrivs(r)
			return js.O("roleid", a.Get("roleid"), "privilege_count", len(privs), "privileges", js.Array(privs), "by_area", accessGroupPrivs(privs)), nil
		},
	})

	c.Define("proxmox_save_role", Def{
		Toolset:     accessToolset,
		Title:       "Create or update a role",
		Description: "Create (create=true) a custom role or change its privileges. On update the privilege list replaces the current one unless append=true. See proxmox_list_privileges for valid names. Built-in roles can't be modified.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("roleid", accessRoleidArg),
			P("create", Bool().Opt().D("Create a new role (default false = update an existing one)")),
			P("privs", Arr(Str()).Opt().D("Privileges, e.g. [\"VM.Audit\", \"VM.PowerMgmt\", \"Datastore.Audit\"]")),
			P("append", Bool().Opt().D("On update, add `privs` to the role instead of replacing its privileges")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			// Without privs, an update would send an empty list and strip every privilege from the role.
			if !a.Truthy("create") && !a.Defined("privs") {
				return nil, errors.New("Give `privs` to update a role ([] removes all privileges)")
			}
			var err error
			if a.Truthy("create") {
				_, err = client.Post(ctx, "/access/roles", defined(js.O("roleid", a.Get("roleid"), "privs", accessJoinList(a.Get("privs")))))
			} else {
				_, err = client.Put(ctx, "/access/roles/"+enc(a.Get("roleid")), defined(js.O("privs", accessJoinList(a.Get("privs")), "append", a.Get("append"))))
			}
			if err != nil {
				return nil, err
			}
			r, err := client.Get(ctx, "/access/roles/"+enc(a.Get("roleid")), nil)
			if err != nil {
				return nil, err
			}
			return js.O(accessCreatedKey(a), a.Get("roleid"), "privileges", js.Array(accessRolePrivs(r))), nil
		},
	})

	c.Define("proxmox_delete_role", Def{
		Toolset:     accessToolset,
		Title:       "Delete a role",
		Description: "Delete a custom role. ACL entries using it stop granting anything. Built-in roles can't be deleted.",
		Delete:      true,
		Input:       []Prop{P("roleid", accessRoleidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/roles/"+enc(a.Get("roleid")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("roleid")), nil
		},
	})

	c.Define("proxmox_list_privileges", Def{
		Toolset:     accessToolset,
		Title:       "List privileges",
		Description: "List every privilege Proxmox knows (derived from the built-in Administrator role, which holds them all), grouped by area. Use when building a custom role with proxmox_save_role.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			r, err := client.Get(ctx, "/access/roles/Administrator", nil)
			if err != nil {
				return nil, err
			}
			privs := accessRolePrivs(r)
			return js.O("count", len(privs), "by_area", accessGroupPrivs(privs)), nil
		},
	})

	// ================================================================ ACL and permissions

	c.Define("proxmox_list_acl", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List ACL entries",
		Description: "List access control entries: which role a user, group or API token has on which path (/, /vms/100, /storage/local, /pool/x, /nodes/pve1...) and whether it propagates to sub-paths. Filters combine. For what a user can effectively do, use proxmox_get_permissions.",
		Input: []Prop{
			P("path", Str().Opt().D("Only entries on this path or below it, e.g. /vms or /vms/100")),
			P("user", Str().Opt().D("Only entries for this user or API token ID (exact)")),
			P("group", Str().Opt().D("Only entries for this group")),
			P("role", Str().Opt().D("Only entries granting this role")),
			P("type", Enum("user", "group", "token").Opt().D("Only entries of this subject type")),
			P("search", searchArg),
			P("limit", limitArg(1000)),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			acl, err := client.GetList(ctx, "/access/acl", nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("path") {
				p := a.Str("path")
				if p != "/" {
					p = strings.TrimRight(p, "/")
				}
				acl = filterObjs(acl, func(e *Obj) bool {
					return p == "/" || e.Get("path") == p || strings.HasPrefix(js.Str(e.Get("path")), p+"/")
				})
			}
			if a.Truthy("user") {
				acl = filterObjs(acl, func(e *Obj) bool { return e.Get("type") != "group" && e.Get("ugid") == a.Get("user") })
			}
			if a.Truthy("group") {
				acl = filterObjs(acl, func(e *Obj) bool { return e.Get("type") == "group" && e.Get("ugid") == a.Get("group") })
			}
			if a.Truthy("role") {
				acl = filterObjs(acl, func(e *Obj) bool { return e.Get("roleid") == a.Get("role") })
			}
			if a.Truthy("type") {
				acl = filterObjs(acl, func(e *Obj) bool { return e.Get("type") == a.Get("type") })
			}
			sortObjs(acl, func(x, y *Obj) float64 {
				if c := compareStr(x.Get("path"), y.Get("path")); c != 0 {
					return c
				}
				return compareStr(x.Get("ugid"), y.Get("ugid"))
			})
			res := filterAndLimit(acl, a.Get("search"), or(a.Get("limit"), 1000), func(e *Obj) []any {
				return []any{e.Get("path"), e.Get("ugid"), e.Get("roleid")}
			})
			return res.Map(func(e *Obj) any {
				return js.O("path", e.Get("path"), "ugid", e.Get("ugid"), "type", e.Get("type"), "role", e.Get("roleid"), "propagate", or(proxmox.Flag(e.Get("propagate")), true))
			}), nil
		},
	})

	c.Define("proxmox_update_acl", Def{
		Toolset:     accessToolset,
		Title:       "Grant or revoke permissions",
		Description: "Grant (default) or revoke (delete=true) roles on a path for users, groups and/or API tokens. Paths: / (everything), /vms/<vmid>, /storage/<id>, /pool/<pool>, /nodes/<node>, /sdn/zones/<zone>, /mapping/... Note privilege-separated API tokens need their own ACL entries (tokens=[\"user@realm!name\"]), and their effective rights are the intersection with the user's.",
		Write:       true,
		Destructive: True,
		Idempotent:  true,
		Input: []Prop{
			P("path", Str().D("ACL path, e.g. /, /vms/100, /storage/local, /pool/dev")),
			P("roles", Arr(Str()).Min(1).D("Roles to grant or revoke, e.g. [\"PVEVMUser\"]")),
			P("users", Arr(Str()).Opt().D("User IDs, e.g. [\"alice@pve\"]")),
			P("groups", Arr(Str()).Opt().D("Group IDs")),
			P("tokens", Arr(Str()).Opt().D("Full API token IDs, e.g. [\"alice@pve!automation\"]")),
			P("propagate", Bool().Opt().D("Inherit to sub-paths (default true)")),
			P("delete", Bool().Opt().D("Revoke the roles instead of granting them")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if accessArrLen(a, "users") == 0 && accessArrLen(a, "groups") == 0 && accessArrLen(a, "tokens") == 0 {
				return nil, errors.New("Give at least one of users, groups or tokens")
			}
			nonEmpty := func(k string) any { return ifThen(accessArrLen(a, k) > 0, accessJoinList(a.Get(k))) }
			if _, err := client.Put(ctx, "/access/acl", defined(js.O(
				"path", a.Get("path"),
				"roles", accessJoinList(a.Get("roles")),
				"users", nonEmpty("users"),
				"groups", nonEmpty("groups"),
				"tokens", nonEmpty("tokens"),
				"propagate", a.Get("propagate"),
				"delete", a.Get("delete"),
			))); err != nil {
				return nil, err
			}
			return proxmox.C(
				pick(a.Truthy("delete"), "revoked", "granted").(string), a.Get("roles"),
				"path", a.Get("path"),
				"users", a.Get("users"),
				"groups", a.Get("groups"),
				"tokens", a.Get("tokens"),
				"propagate", ifThen(!a.Truthy("delete"), or(a.Get("propagate"), true)),
			), nil
		},
	})

	c.Define("proxmox_get_permissions", Def{
		Toolset:     accessToolset,
		Title:       "Get effective permissions",
		Description: "Show effective privileges per path for the API token/user this server uses (default) or for another user or token. The first thing to check when a tool fails with 403 / 'Permission check failed': it shows exactly which privileges are held where. Use `path` to check a single path such as /vms/100.",
		Input: []Prop{
			P("userid", Str().Opt().D("User or full API token ID (user@realm!token) to check (default: the caller)")),
			P("path", Str().Opt().D("Only this path, e.g. /vms/100 or /storage/local")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			perms, err := client.GetObj(ctx, "/access/permissions", defined(js.O("userid", a.Get("userid"), "path", a.Get("path"))))
			if err != nil {
				return nil, err
			}
			paths := perms.Keys()
			sort.Strings(paths)
			out := js.NewObj()
			for _, p := range paths {
				entries := accessObjOrEmpty(perms.Get(p))
				keys := entries.Keys()
				sort.SliceStable(keys, func(i, j int) bool { return js.LocaleCompare(keys[i], keys[j]) < 0 })
				var privs, nonProp []any
				for _, k := range keys {
					privs = append(privs, k)
					// The value is the propagate flag: privileges that don't propagate only apply to this exact path.
					if !js.Truthy(proxmox.Flag(entries.Get(k))) {
						nonProp = append(nonProp, k)
					}
				}
				out.Set(p, proxmox.C("privileges", js.V(privs), "not_propagated", js.V(nonProp)))
			}
			cfg := client.Config()
			subject := a.Get("userid")
			if js.Nullish(subject) {
				switch {
				case cfg.TokenID != "":
					subject = cfg.TokenID
				case cfg.Username != "":
					subject = cfg.Username
				default:
					subject = "(caller)"
				}
			}
			return js.O("subject", subject, "paths", out), nil
		},
	})

	// ================================================================ API tokens

	c.Define("proxmox_list_api_tokens", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List API tokens",
		Description: "List API tokens (full ID user@realm!name, privilege separation, expiry, comment) for one user, or for all users when userid is omitted. Secrets can never be read back.",
		Input:       []Prop{P("userid", accessUseridArg.Opt()), P("search", searchArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			var tokens []*Obj
			if a.Truthy("userid") {
				list, err := client.GetList(ctx, "/access/users/"+enc(a.Get("userid"))+"/token", nil)
				if err != nil {
					return nil, err
				}
				for _, t := range list {
					tokens = append(tokens, accessSummarizeToken(a.Get("userid"), t.Get("tokenid"), t))
				}
			} else {
				users, err := client.GetList(ctx, "/access/users", js.O("full", 1))
				if err != nil {
					return nil, err
				}
				for _, u := range users {
					for _, t := range u.Objs("tokens") {
						tokens = append(tokens, accessSummarizeToken(u.Get("userid"), t.Get("tokenid"), t))
					}
				}
			}
			sortObjs(tokens, func(x, y *Obj) float64 { return compareStr(x.Get("id"), y.Get("id")) })
			return filterAndLimit(tokens, a.Get("search"), js.Undef, func(t *Obj) []any { return []any{t.Get("id"), t.Get("comment")} }).Obj(nil), nil
		},
	})

	c.Define("proxmox_get_api_token", Def{
		Toolset:     accessToolset,
		Title:       "Get API token",
		Description: "Get one API token's settings (privilege separation, expiry, comment) and, when privilege-separated, its own ACL entries.",
		Input:       []Prop{P("userid", accessUseridArg), P("tokenid", accessTokenidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			t, err := client.GetObj(ctx, "/access/users/"+enc(a.Get("userid"))+"/token/"+enc(a.Get("tokenid")), nil)
			if err != nil {
				return nil, err
			}
			id := js.Str(a.Get("userid")) + "!" + js.Str(a.Get("tokenid"))
			acl, err := client.GetList(ctx, "/access/acl", nil)
			if err != nil {
				acl = nil
			}
			entries := filterObjs(acl, func(e *Obj) bool { return e.Get("type") == "token" && e.Get("ugid") == id })
			return proxmox.Compact(js.Spread(
				accessSummarizeToken(a.Get("userid"), a.Get("tokenid"), t),
				js.O("acl", mapObjs(entries, func(e *Obj) any {
					return js.O("path", e.Get("path"), "role", e.Get("roleid"), "propagate", or(proxmox.Flag(e.Get("propagate")), true))
				})),
			)), nil
		},
	})

	c.Define("proxmox_create_api_token", Def{
		Toolset:     accessToolset,
		Title:       "Create an API token",
		Description: "Create an API token for a user. The result contains the token SECRET, which Proxmox shows only this once: pass it on to the user to store safely. With privsep=true (default) the token has no rights until granted with proxmox_update_acl (tokens=[...]); with privsep=false it has the user's full rights.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("userid", accessUseridArg),
			P("tokenid", accessTokenidArg),
			P("comment", Str().Opt().D("Comment")),
			P("expire", accessExpireArg),
			P("privsep", Bool().Opt().D("Privilege separation: restrict the token with its own ACLs (default true)")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			expire, err := accessToEpoch(a.Get("expire"))
			if err != nil {
				return nil, err
			}
			res, err := client.Post(ctx, "/access/users/"+enc(a.Get("userid"))+"/token/"+enc(a.Get("tokenid")),
				defined(js.O("comment", a.Get("comment"), "expire", expire, "privsep", a.Get("privsep"))))
			if err != nil {
				return nil, err
			}
			r := accessAsObj(res)
			id := or(r.Get("full-tokenid"), js.Str(a.Get("userid"))+"!"+js.Str(a.Get("tokenid")))
			info := r.ObjAt("info")
			// Deliberately not redacted: this is the only time the secret is available.
			return proxmox.C(
				"created", id,
				"token_id", id,
				"secret", r.Get("value"),
				"authorization_header", ifThen(r.Truthy("value"), "PVEAPIToken="+js.Str(id)+"="+js.Str(r.Get("value"))),
				"privsep", or(or(proxmox.Flag(info.Get("privsep")), a.Get("privsep")), true),
				"expire", accessExpiry(info.Get("expire")),
				"note", "Store the secret now: Proxmox cannot show it again. If lost, delete and recreate the token.",
			), nil
		},
	})

	c.Define("proxmox_update_api_token", Def{
		Toolset:     accessToolset,
		Title:       "Update an API token",
		Description: "Change an API token's comment, expiry or privilege separation. regenerate=true issues a new secret (returned once, store it) and immediately invalidates the old one.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("userid", accessUseridArg),
			P("tokenid", accessTokenidArg),
			P("comment", Str().Opt().D("Comment")),
			P("expire", accessExpireArg),
			P("privsep", Bool().Opt().D("Privilege separation (true = token limited to its own ACLs)")),
			P("regenerate", Bool().Opt().D("Generate a new secret; everything using the old one loses access")),
			P("delete", deleteKeysArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			expire, err := accessToEpoch(a.Get("expire"))
			if err != nil {
				return nil, err
			}
			res, err := client.Put(ctx, "/access/users/"+enc(a.Get("userid"))+"/token/"+enc(a.Get("tokenid")),
				defined(js.O("comment", a.Get("comment"), "expire", expire, "privsep", a.Get("privsep"), "regenerate", a.Get("regenerate"), "delete", deleteParam(a.Get("delete")))))
			if err != nil {
				return nil, err
			}
			r := accessAsObj(res)
			id := js.Str(a.Get("userid")) + "!" + js.Str(a.Get("tokenid"))
			hasValue := r.Truthy("value")
			return proxmox.C(
				"updated", id,
				"token", accessSummarizeToken(a.Get("userid"), a.Get("tokenid"), accessObjOrEmpty(res)),
				"secret", r.Get("value"),
				"authorization_header", ifThen(hasValue, "PVEAPIToken="+id+"="+js.Str(r.Get("value"))),
				"note", ifThen(hasValue, "New secret shown once: store it now. The old secret no longer works."),
			), nil
		},
	})

	c.Define("proxmox_delete_api_token", Def{
		Toolset:     accessToolset,
		Title:       "Delete an API token",
		Description: "Delete (revoke) an API token; anything using it loses access immediately.",
		Delete:      true,
		Input:       []Prop{P("userid", accessUseridArg), P("tokenid", accessTokenidArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/users/"+enc(a.Get("userid"))+"/token/"+enc(a.Get("tokenid")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", js.Str(a.Get("userid"))+"!"+js.Str(a.Get("tokenid"))), nil
		},
	})

	// ================================================================ realms

	c.Define("proxmox_list_realms", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List realms",
		Description: "List authentication realms (pam, pve, LDAP, Active Directory, OpenID Connect) with type, TFA provider and comment.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			realms, err := client.GetList(ctx, "/access/domains", nil)
			if err != nil {
				return nil, err
			}
			return mapObjs(realms, func(r *Obj) any {
				return proxmox.C("realm", r.Get("realm"), "type", r.Get("type"), "tfa", accessRedactTfa(r.Get("tfa")), "comment", r.Get("comment"))
			}), nil
		},
	})

	c.Define("proxmox_get_realm", Def{
		Toolset:     accessToolset,
		Title:       "Get realm",
		Description: "Get an authentication realm's configuration (servers, base DN, sync options, OpenID issuer...). Bind passwords and client keys are redacted.",
		Input:       []Prop{P("realm", accessRealmArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			cfg, err := client.Get(ctx, "/access/domains/"+enc(a.Get("realm")), nil)
			if err != nil {
				return nil, err
			}
			return accessRedactRealm(js.Spread(js.O("realm", a.Get("realm")), cfg)), nil
		},
	})

	c.Define("proxmox_save_realm", Def{
		Toolset:     accessToolset,
		Title:       "Create or update a realm",
		Description: "Create (create=true, with `type`) or update an authentication realm: LDAP/AD (server1, base_dn, user_attr, bind_dn + bind_password, mode), OpenID Connect (issuer_url, client_id, client_key, username_claim) or pam/pve (comment, default, tfa). Only the given fields change on update. Anything else via `extra` (e.g. group_dn, filter, sync_attributes, groups-claim). Secrets are never echoed back.",
		Write:       true,
		Destructive: True,
		Input: []Prop{
			P("realm", accessRealmArg),
			P("create", Bool().Opt().D("Create a new realm (default false = update)")),
			P("type", Enum("pam", "pve", "ldap", "ad", "openid").Opt().D("Realm type (required when creating)")),
			P("comment", Str().Opt().D("Description shown on the login screen")),
			P("default", Bool().Opt().D("Make this the default realm on the login screen")),
			P("tfa", Str().Opt().D("Realm-enforced TFA, e.g. 'type=oath' or 'type=yubico,id=...,key=...'")),
			P("server1", Str().Opt().D("LDAP/AD server address")),
			P("server2", Str().Opt().D("Fallback LDAP/AD server")),
			P("port", Int().Pos().Opt().D("LDAP/AD server port")),
			P("mode", Enum("ldap", "ldaps", "ldap+starttls").Opt().D("LDAP/AD connection mode")),
			P("verify", Bool().Opt().D("Verify the LDAP server's TLS certificate")),
			P("base_dn", Str().Opt().D("LDAP base DN for users, e.g. ou=people,dc=example,dc=com")),
			P("user_attr", Str().Opt().D("LDAP user name attribute, e.g. uid (required for LDAP)")),
			P("bind_dn", Str().Opt().D("LDAP bind DN")),
			P("bind_password", Str().Opt().D("LDAP bind password (sent as `password`; never echoed)")),
			P("domain", Str().Opt().D("AD domain, e.g. example.com (required for AD)")),
			P("issuer_url", Str().Opt().D("OpenID issuer URL")),
			P("client_id", Str().Opt().D("OpenID client ID")),
			P("client_key", Str().Opt().D("OpenID client secret (never echoed)")),
			P("username_claim", Str().Opt().D("OpenID claim used for the username (create only), e.g. email or preferred_username")),
			P("scopes", Str().Opt().D("OpenID scopes (default 'email profile')")),
			P("autocreate", Bool().Opt().D("Automatically create users on first login (OpenID)")),
			P("sync_defaults_options", Str().Opt().D("Default sync options, e.g. 'scope=both,enable-new=1,remove-vanished=acl;entry'")),
			P("delete", deleteKeysArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params, err := definedExtra(js.O(
				"comment", a.Get("comment"),
				"default", a.Get("default"),
				"tfa", a.Get("tfa"),
				"server1", a.Get("server1"),
				"server2", a.Get("server2"),
				"port", a.Get("port"),
				"mode", a.Get("mode"),
				"verify", a.Get("verify"),
				"base_dn", a.Get("base_dn"),
				"user_attr", a.Get("user_attr"),
				"bind_dn", a.Get("bind_dn"),
				"password", a.Get("bind_password"),
				"domain", a.Get("domain"),
				"issuer-url", a.Get("issuer_url"),
				"client-id", a.Get("client_id"),
				"client-key", a.Get("client_key"),
				"username-claim", a.Get("username_claim"),
				"scopes", a.Get("scopes"),
				"autocreate", a.Get("autocreate"),
				"sync-defaults-options", a.Get("sync_defaults_options"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			if a.Truthy("create") {
				if !a.Truthy("type") {
					return nil, errors.New("type is required when creating a realm")
				}
				_, err = client.Post(ctx, "/access/domains", js.Spread(js.O("realm", a.Get("realm"), "type", a.Get("type")), params))
			} else {
				_, err = client.Put(ctx, "/access/domains/"+enc(a.Get("realm")), js.Spread(params, defined(js.O("delete", deleteParam(a.Get("delete")), "digest", a.Get("digest")))))
			}
			if err != nil {
				return nil, err
			}
			var realm any = js.Undef
			if cfg, err := client.Get(ctx, "/access/domains/"+enc(a.Get("realm")), nil); err == nil && js.Truthy(cfg) {
				realm = accessRedactRealm(cfg)
			}
			return proxmox.C(accessCreatedKey(a), a.Get("realm"), "realm", realm), nil
		},
	})

	c.Define("proxmox_delete_realm", Def{
		Toolset:     accessToolset,
		Title:       "Delete a realm",
		Description: "Delete an authentication realm. Users of that realm can no longer log in (their user entries remain until deleted). pam and pve can't be deleted.",
		Delete:      true,
		Input:       []Prop{P("realm", accessRealmArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/domains/"+enc(a.Get("realm")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("realm")), nil
		},
	})

	c.Define("proxmox_sync_realm", Def{
		Toolset:      accessToolset,
		Title:        "Sync users/groups from a realm",
		NativeDryRun: true,
		Description:  "Sync users and/or groups from an LDAP, AD or OpenID realm into Proxmox. Use dry_run=true first to see what would change (in the task log). Options not given fall back to the realm's sync-defaults-options. Synced groups are named <group>-<realm>.",
		Write:        true,
		Destructive:  True,
		Input: []Prop{
			P("realm", accessRealmArg),
			P("dry_run", Bool().Opt().D("Only report what would change; write nothing")),
			P("scope", Enum("users", "groups", "both").Opt().D("What to sync")),
			P("enable_new", Bool().Opt().D("Enable newly synced users (default true)")),
			P("remove_vanished", Str().Opt().D("What to remove for users/groups no longer in the directory: 'none' or a ;-list of acl, properties, entry (e.g. 'acl;entry')")),
			P("wait", waitArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			upid, err := client.Post(ctx, "/access/domains/"+enc(a.Get("realm"))+"/sync",
				defined(js.O("dry-run", a.Get("dry_run"), "scope", a.Get("scope"), "enable-new", a.Get("enable_new"), "remove-vanished", a.Get("remove_vanished"))))
			if err != nil {
				return nil, err
			}
			return runTask(ctx, client, upid, a.Get("wait"), proxmox.C("realm", a.Get("realm"), "dry_run", orUndef(a.Get("dry_run"))))
		},
	})

	// ================================================================ TFA

	c.Define("proxmox_list_tfa", Def{
		Toolset:     accessToolset,
		List:        &listing{Items: "entries"},
		Title:       "List TFA entries",
		Description: "List users' second factors (TOTP, WebAuthn, recovery keys, Yubico) with type, description, creation time and enabled state, plus TFA lock-outs. No secrets are returned. Unlock a locked-out user with proxmox_unlock_tfa.",
		Input:       []Prop{P("userid", accessUseridArg.Opt())},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if a.Truthy("userid") {
				entries, err := client.GetList(ctx, "/access/tfa/"+enc(a.Get("userid")), nil)
				if err != nil {
					return nil, err
				}
				return js.O("userid", a.Get("userid"), "entries", mapObjs(entries, accessSummarizeTfaEntry)), nil
			}
			all, err := client.GetList(ctx, "/access/tfa", nil)
			if err != nil {
				return nil, err
			}
			out := make([]*Obj, 0, len(all))
			for _, u := range all {
				lockedUntil := u.Num("tfa-locked-until")
				out = append(out, proxmox.C(
					"userid", u.Get("userid"),
					"totp_locked", orUndef(proxmox.Flag(u.Get("totp-locked"))),
					"tfa_locked_until", ifThen(lockedUntil > accessNowSeconds(), proxmox.IsoTime(lockedUntil)),
					"entries", mapObjs(js.Objs(or(u.Get("entries"), []any{})), accessSummarizeTfaEntry),
				))
			}
			sortObjs(out, func(x, y *Obj) float64 { return compareStr(x.Get("userid"), y.Get("userid")) })
			return js.Array(out), nil
		},
	})

	c.Define("proxmox_delete_tfa", Def{
		Toolset:     accessToolset,
		Title:       "Delete a TFA entry",
		Description: "Remove one second factor from a user (e.g. a lost authenticator); get the ID from proxmox_list_tfa. Removing a user's last factor lets them log in with the password alone.",
		Delete:      true,
		Input: []Prop{
			P("userid", accessUseridArg),
			P("id", Str().D("TFA entry ID (from proxmox_list_tfa)")),
			P("password", Str().Opt().D("Current password of the user making the change, if Proxmox requires it")),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/access/tfa/"+enc(a.Get("userid"))+"/"+enc(a.Get("id")), defined(js.O("password", a.Get("password")))); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id"), "userid", a.Get("userid")), nil
		},
	})

	c.Define("proxmox_unlock_tfa", Def{
		Toolset:     accessToolset,
		Title:       "Unlock a user's TFA",
		Description: "Clear a user's TFA lock-out after too many failed second-factor attempts (see totp_locked / tfa_locked_until in proxmox_list_tfa).",
		Write:       true,
		Destructive: False,
		Idempotent:  true,
		Input:       []Prop{P("userid", accessUseridArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			r, err := client.Put(ctx, "/access/users/"+enc(a.Get("userid"))+"/unlock-tfa", nil)
			if err != nil {
				return nil, err
			}
			return js.O("userid", a.Get("userid"), "was_locked", proxmox.Flag(r), "unlocked", true), nil
		},
	})

	// ================================================================ realm sync jobs

	c.Define("proxmox_list_realm_sync_jobs", Def{
		Toolset:     accessToolset,
		List:        &listing{},
		Title:       "List realm sync jobs",
		Description: "List scheduled realm sync jobs (periodic LDAP/AD/OpenID user and group sync) with schedule, last and next run.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			jobs, err := client.GetList(ctx, "/cluster/jobs/realm-sync", nil)
			if err != nil {
				return nil, err
			}
			return mapObjs(jobs, func(j *Obj) any {
				return proxmox.C(
					"id", j.Get("id"),
					"realm", j.Get("realm"),
					"enabled", or(proxmox.Flag(j.Get("enabled")), true),
					"schedule", j.Get("schedule"),
					"scope", j.Get("scope"),
					"remove_vanished", j.Get("remove-vanished"),
					"last_run", proxmox.IsoTime(j.Get("last-run")),
					"next_run", proxmox.IsoTime(j.Get("next-run")),
					"comment", j.Get("comment"),
				)
			}), nil
		},
	})

	c.Define("proxmox_save_realm_sync_job", Def{
		Toolset:     accessToolset,
		Title:       "Create or update a realm sync job",
		Description: "Create (create=true, with realm and schedule) or update a scheduled realm sync job. Check schedules with proxmox_simulate_schedule. For a one-off sync use proxmox_sync_realm.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("id", Str().D("Job ID")),
			P("create", Bool().Opt().D("Create a new job (default false = update)")),
			P("realm", Str().Opt().D("Realm to sync (create only)")),
			P("schedule", Str().Opt().D("Calendar event, e.g. 'daily' or 'sun 03:00' (required when creating)")),
			P("enabled", Bool().Opt().D("Whether the job runs")),
			P("scope", Enum("users", "groups", "both").Opt().D("What to sync")),
			P("enable_new", Bool().Opt().D("Enable newly synced users")),
			P("remove_vanished", Str().Opt().D("'none' or a ;-list of acl, properties, entry")),
			P("comment", Str().Opt().D("Comment")),
			P("delete", deleteKeysArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params := defined(js.O(
				"enabled", a.Get("enabled"),
				"scope", a.Get("scope"),
				"enable-new", a.Get("enable_new"),
				"remove-vanished", a.Get("remove_vanished"),
				"comment", a.Get("comment"),
			))
			path := "/cluster/jobs/realm-sync/" + enc(a.Get("id"))
			if a.Truthy("create") {
				if !a.Truthy("realm") || !a.Truthy("schedule") {
					return nil, errors.New("realm and schedule are required when creating a realm sync job")
				}
				if _, err := client.Post(ctx, path, js.Spread(js.O("realm", a.Get("realm"), "schedule", a.Get("schedule")), params)); err != nil {
					return nil, err
				}
			} else {
				// PUT requires the schedule; keep the current one when not changing it.
				schedule := a.Get("schedule")
				if js.Nullish(schedule) {
					cur, err := client.GetObj(ctx, path, nil)
					if err != nil {
						return nil, err
					}
					schedule = cur.Get("schedule")
				}
				if _, err := client.Put(ctx, path, js.Spread(js.O("schedule", schedule), params, defined(js.O("delete", deleteParam(a.Get("delete")))))); err != nil {
					return nil, err
				}
			}
			job, err := client.Get(ctx, path, nil)
			if err != nil {
				job = js.Undef
			}
			return js.O(accessCreatedKey(a), a.Get("id"), "job", job), nil
		},
	})

	c.Define("proxmox_delete_realm_sync_job", Def{
		Toolset:     accessToolset,
		Title:       "Delete a realm sync job",
		Description: "Delete a scheduled realm sync job (the realm and synced users stay).",
		Delete:      true,
		Input:       []Prop{P("id", Str().D("Job ID"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, "/cluster/jobs/realm-sync/"+enc(a.Get("id")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("id")), nil
		},
	})
}

// accessObjOrEmpty returns v when it is an object, else an empty object (like `x ?? {}` for API data).
func accessObjOrEmpty(v any) *Obj {
	if o, ok := v.(*Obj); ok && o != nil {
		return o
	}
	return js.NewObj()
}
