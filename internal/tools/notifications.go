package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

const (
	notificationsToolset  = "notifications"
	notificationsBase     = "/cluster/notifications"
	notificationsRedacted = "<redacted>"
)

var notificationsEndpointTypes = []string{"sendmail", "smtp", "gotify", "webhook"}

var (
	notificationsNameArg         = Str().D("Endpoint/target name (see proxmox_list_notification_targets)")
	notificationsMatcherNameArg  = Str().D("Matcher name (see proxmox_list_notification_matchers)")
	notificationsEndpointTypeArg = Enum(notificationsEndpointTypes...).D("Endpoint type: sendmail (local mail), smtp, gotify or webhook")
	notificationsDeleteArrayArg  = Arr(Str()).Opt().D("Settings to remove/reset, e.g. [\"comment\", \"mailto-user\"] (Proxmox names)")

	// notificationsSensitiveHeaderRe matches header names whose values are credentials, so
	// webhook outputs hide them.
	notificationsSensitiveHeaderRe = regexp.MustCompile(`(?i)auth|token|key|secret|cookie|pass|signature`)
)

// notificationsB64Decode decodes like Buffer.from(value, "base64").toString("utf8"): characters
// outside the (standard or URL-safe) alphabet are skipped, decoding stops at padding, and
// invalid UTF-8 becomes U+FFFD. Undefined stays undefined.
func notificationsB64Decode(value any) any {
	if js.IsUndef(value) {
		return js.Undef
	}
	s := js.Str(value)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '=':
			i = len(s)
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			b.WriteByte(c)
		case c == '-':
			b.WriteByte('+')
		case c == '_':
			b.WriteByte('/')
		}
	}
	clean := b.String()
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	data, _ := base64.RawStdEncoding.DecodeString(clean)
	return strings.ToValidUTF8(string(data), "�")
}

func notificationsB64(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }

// notificationsDecodePairs turns "name=X,value=<base64>" entries into {X: decoded value} with
// credentials redacted (undefined for an empty or missing list).
func notificationsDecodePairs(list any, redactAll bool) any {
	arr, ok := list.([]any)
	if !ok || len(arr) == 0 {
		return js.Undef
	}
	out := js.NewObj()
	for _, item := range arr {
		p := proxmox.ParsePropertyString(item, "")
		name := js.Str(or(p.Get("name"), or(p.Get("_default"), "?")))
		if redactAll || notificationsSensitiveHeaderRe.MatchString(name) {
			out.Set(name, pick(p.Defined("value") || redactAll, notificationsRedacted, ""))
		} else {
			out.Set(name, or(notificationsB64Decode(p.Get("value")), ""))
		}
	}
	return out
}

// notificationsRedactEndpoint is an endpoint config with secrets hidden and webhook
// header/body/secret fields decoded.
func notificationsRedactEndpoint(e *Obj) *Obj {
	out, _ := proxmox.RedactSecrets(e.Clone()).(*Obj)
	for _, k := range []string{"token", "password"} {
		if out.Defined(k) {
			out.Set(k, notificationsRedacted)
		}
	}
	if e.Defined("header") {
		out.Set("header", notificationsDecodePairs(e.Get("header"), false))
	}
	if e.Defined("secret") {
		out.Set("secret", notificationsDecodePairs(e.Get("secret"), true))
	}
	if body, ok := e.Get("body").(string); ok && body != "" {
		out.Set("body", notificationsB64Decode(body))
	}
	return out
}

func notificationsSummarizeMatcher(m *Obj) *Obj {
	return proxmox.C(
		"name", m.Get("name"),
		"disabled", orUndef(proxmox.Flag(m.Get("disable"))),
		"origin", m.Get("origin"),
		"mode", or(m.Get("mode"), "all"),
		"invert_match", orUndef(proxmox.Flag(m.Get("invert-match"))),
		"match_severity", m.Get("match-severity"),
		"match_field", m.Get("match-field"),
		"match_calendar", m.Get("match-calendar"),
		"targets", m.Get("target"),
		"comment", m.Get("comment"),
	)
}

func notificationsSortByName(items []*Obj) []any {
	sortObjs(items, func(x, y *Obj) float64 { return compareStr(js.Str(x.Get("name")), js.Str(y.Get("name"))) })
	return js.Array(items)
}

// notificationsDeleteDigest is defined({delete: a.delete?.length ? a.delete : undefined, digest: a.digest}).
func notificationsDeleteDigest(a *Obj) *Obj {
	var del any = js.Undef
	if arr := a.Arr("delete"); len(arr) > 0 {
		del = arr
	}
	return defined(js.O("delete", del, "digest", a.Get("digest")))
}

func registerNotifications(c *Context) {
	client := c.Client

	endpointType := func(ctx context.Context, name string, typ any) (string, error) {
		if js.Truthy(typ) {
			return js.Str(typ), nil
		}
		targets, err := client.GetList(ctx, notificationsBase+"/targets", nil)
		if err != nil {
			return "", err
		}
		for _, t := range targets {
			if t.Get("name") == name {
				return js.Str(t.Get("type")), nil
			}
		}
		return "", fmt.Errorf(`No notification target named "%s" (see proxmox_list_notification_targets)`, name)
	}

	// ---------------------------------------------------------------- targets / endpoints

	c.Define("proxmox_list_notification_targets", Def{
		Toolset:     notificationsToolset,
		Title:       "List notification targets",
		Description: "List all notification targets (endpoints of every type: sendmail, smtp, gotify, webhook) with type, disabled state, origin and comment. details=true adds each endpoint's configuration (secrets redacted). Matchers decide which notifications go to which target; see proxmox_list_notification_matchers.",
		Input:       []Prop{P("details", Bool().Opt().D("Include each endpoint's full configuration (one extra call per type)"))},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			targets, err := client.GetList(ctx, notificationsBase+"/targets", nil)
			if err != nil {
				return nil, err
			}
			config := map[string]*Obj{}
			if a.Truthy("details") {
				var types []string
				seen := map[string]bool{}
				for _, t := range targets {
					typ, isStr := t.Get("type").(string)
					if !isStr || seen[typ] {
						continue
					}
					seen[typ] = true
					if contains(notificationsEndpointTypes, typ) {
						types = append(types, typ)
					}
				}
				for _, typ := range types {
					list, err := client.GetList(ctx, notificationsBase+"/endpoints/"+typ, nil)
					if err != nil {
						list = nil
					}
					for _, e := range list {
						config[typ+"/"+js.Str(e.Get("name"))] = e
					}
				}
			}
			out := make([]*Obj, 0, len(targets))
			for _, t := range targets {
				var rest any = js.Undef
				if cfg := config[js.Str(t.Get("type"))+"/"+js.Str(t.Get("name"))]; cfg != nil {
					r := notificationsRedactEndpoint(cfg)
					for _, k := range []string{"name", "comment", "disable", "origin"} {
						r.Delete(k)
					}
					rest = r
				}
				out = append(out, proxmox.C("name", t.Get("name"), "type", t.Get("type"), "disabled", orUndef(proxmox.Flag(t.Get("disable"))), "origin", t.Get("origin"), "comment", t.Get("comment"), "config", rest))
			}
			return notificationsSortByName(out), nil
		},
	})

	c.Define("proxmox_get_notification_endpoint", Def{
		Toolset:     notificationsToolset,
		Title:       "Get notification endpoint",
		Description: "Get one notification endpoint's configuration. The type is looked up when omitted. Tokens, passwords and webhook secrets are redacted; webhook headers and body are shown decoded (credential-like headers redacted).",
		Input:       []Prop{P("name", notificationsNameArg), P("type", notificationsEndpointTypeArg.Opt()), P("raw", rawArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			typ, err := endpointType(ctx, a.Str("name"), a.Get("type"))
			if err != nil {
				return nil, err
			}
			e, err := client.GetObj(ctx, notificationsBase+"/endpoints/"+typ+"/"+enc(a.Get("name")), nil)
			if err != nil {
				return nil, err
			}
			if a.Truthy("raw") {
				// Keep the raw property strings, but blank the base64 values of credential-like headers (e.g. Authorization).
				header := e.Get("header")
				if arr, ok := header.([]any); ok {
					mapped := make([]any, len(arr))
					for i, h := range arr {
						p := proxmox.ParsePropertyString(h, "")
						if notificationsSensitiveHeaderRe.MatchString(js.Str(or(p.Get("name"), ""))) && p.Defined("value") {
							mapped[i] = "name=" + js.Str(p.Get("name")) + ",value=" + notificationsRedacted
						} else {
							mapped[i] = h
						}
					}
					header = mapped
				}
				out := js.O("type", typ).Assign(e).Set("header", header)
				if e.Truthy("secret") {
					out.Set("secret", notificationsDecodePairs(e.Get("secret"), true))
				}
				return proxmox.RedactSecrets(out), nil
			}
			out := js.O("type", typ).Assign(notificationsRedactEndpoint(e))
			out.Set("disable", js.Undef)
			out.Set("disabled", orUndef(proxmox.Flag(e.Get("disable"))))
			return proxmox.Compact(out), nil
		},
	})

	c.Define("proxmox_save_notification_endpoint", Def{
		Toolset: notificationsToolset,
		Title:   "Create or update a notification endpoint",
		Description: "Create (create=true) or update a notification endpoint of any type. Only the given fields change on update. " +
			"sendmail: mailto and/or mailto_user, from_address, author. " +
			"smtp: server, from_address (required), port, mode, username, password, mailto/mailto_user, author. " +
			"gotify: server URL and token (required). " +
			"webhook: url and method (required), headers, body (plain text; Proxmox templates like {{ title }} / {{ message }} work), secrets (referenced as {{ secrets.<name> }}). " +
			"Header, body and secret values are base64-encoded for you. Afterwards route notifications to it with proxmox_save_notification_matcher and try it with proxmox_test_notification_target.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("type", notificationsEndpointTypeArg),
			P("name", notificationsNameArg),
			P("create", Bool().Opt().D("Create a new endpoint (default false = update)")),
			P("comment", Str().Opt().D("Comment")),
			P("disable", Bool().Opt().D("Disable the endpoint")),
			P("mailto", Arr(Str()).Opt().D("sendmail/smtp: recipient email addresses")),
			P("mailto_user", Arr(Str()).Opt().D("sendmail/smtp: users whose configured email is used, e.g. [\"root@pam\"]")),
			P("from_address", Str().Opt().D("sendmail/smtp: From address (required for smtp)")),
			P("author", Str().Opt().D("sendmail/smtp: author name (default 'Proxmox VE')")),
			P("server", Str().Opt().D("smtp: server host name; gotify: server URL")),
			P("port", Int().Pos().Opt().D("smtp: port (default 465 tls, 587 starttls, 25 insecure)")),
			P("mode", Enum("insecure", "starttls", "tls").Opt().D("smtp: encryption (default tls)")),
			P("username", Str().Opt().D("smtp: login user name")),
			P("password", Str().Opt().D("smtp: login password (never echoed)")),
			P("token", Str().Opt().D("gotify: application token (never echoed)")),
			P("url", Str().Opt().D("webhook: URL (may use {{ secrets.<name> }})")),
			P("method", Enum("post", "put", "get").Opt().D("webhook: HTTP method")),
			P("headers", Rec(Str()).Opt().D("webhook: HTTP headers as {name: value} (replaces all headers)")),
			P("body", Str().Opt().D("webhook: request body template in plain text")),
			P("secrets", Rec(Str()).Opt().D("webhook: secrets as {name: value} (replaces all secrets; never echoed)")),
			P("delete", notificationsDeleteArrayArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			t := a.Str("type")
			mail := t == "sendmail" || t == "smtp"
			checks := []struct {
				key string
				bad bool
			}{
				{"mailto", a.Defined("mailto") && !mail},
				{"mailto_user", a.Defined("mailto_user") && !mail},
				{"from_address", a.Defined("from_address") && !mail},
				{"author", a.Defined("author") && !mail},
				{"server", a.Defined("server") && t != "smtp" && t != "gotify"},
				{"port", a.Defined("port") && t != "smtp"},
				{"mode", a.Defined("mode") && t != "smtp"},
				{"username", a.Defined("username") && t != "smtp"},
				{"password", a.Defined("password") && t != "smtp"},
				{"token", a.Defined("token") && t != "gotify"},
				{"url", a.Defined("url") && t != "webhook"},
				{"method", a.Defined("method") && t != "webhook"},
				{"headers", a.Defined("headers") && t != "webhook"},
				{"body", a.Defined("body") && t != "webhook"},
				{"secrets", a.Defined("secrets") && t != "webhook"},
			}
			var misplaced []string
			for _, ch := range checks {
				if ch.bad {
					misplaced = append(misplaced, ch.key)
				}
			}
			if len(misplaced) > 0 {
				return nil, fmt.Errorf("Not valid for %s endpoints: %s", t, strings.Join(misplaced, ", "))
			}
			create := a.Truthy("create")
			if create {
				required := map[string][]string{
					"sendmail": nil,
					"smtp":     {"server", "from_address"},
					"gotify":   {"server", "token"},
					"webhook":  {"url", "method"},
				}
				var missing []string
				for _, k := range required[t] {
					if !a.Defined(k) {
						missing = append(missing, k)
					}
				}
				if t == "sendmail" && len(a.Arr("mailto")) == 0 && len(a.Arr("mailto_user")) == 0 {
					missing = append(missing, "mailto or mailto_user")
				}
				if len(missing) > 0 {
					return nil, fmt.Errorf("Required to create a %s endpoint: %s", t, strings.Join(missing, ", "))
				}
			}
			pairs := func(k string) any {
				rec := a.ObjAt(k)
				if !a.Truthy(k) || rec == nil {
					return js.Undef
				}
				out := []any{}
				for _, name := range rec.Keys() {
					out = append(out, "name="+name+",value="+notificationsB64(js.Str(rec.Get(name))))
				}
				return out
			}
			var body any = js.Undef
			if a.Defined("body") {
				body = notificationsB64(a.Str("body"))
			}
			params, err := definedExtra(js.O(
				"comment", a.Get("comment"),
				"disable", a.Get("disable"),
				"mailto", a.Get("mailto"),
				"mailto-user", a.Get("mailto_user"),
				"from-address", a.Get("from_address"),
				"author", a.Get("author"),
				"server", a.Get("server"),
				"port", a.Get("port"),
				"mode", a.Get("mode"),
				"username", a.Get("username"),
				"password", a.Get("password"),
				"token", a.Get("token"),
				"url", a.Get("url"),
				"method", a.Get("method"),
				"header", pairs("headers"),
				"body", body,
				"secret", pairs("secrets"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			path := notificationsBase + "/endpoints/" + t + "/" + enc(a.Get("name"))
			if create {
				_, err = client.Post(ctx, notificationsBase+"/endpoints/"+t, js.Spread(js.O("name", a.Get("name")), params))
			} else {
				_, err = client.Put(ctx, path, js.Spread(params, notificationsDeleteDigest(a)))
			}
			if err != nil {
				return nil, err
			}
			var endpoint any = js.Undef
			if saved, err := client.Get(ctx, path, nil); err == nil {
				if o, ok := saved.(*Obj); ok && o != nil {
					endpoint = notificationsRedactEndpoint(o)
				}
			}
			return proxmox.C(pick(create, "created", "updated").(string), a.Get("name"), "type", t, "endpoint", endpoint), nil
		},
	})

	c.Define("proxmox_delete_notification_endpoint", Def{
		Toolset:     notificationsToolset,
		Title:       "Delete a notification endpoint",
		Description: "Delete a notification endpoint (the type is looked up when omitted). Remove it from matchers first, or they will reference a missing target.",
		Delete:      true,
		Input:       []Prop{P("name", notificationsNameArg), P("type", notificationsEndpointTypeArg.Opt())},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			typ, err := endpointType(ctx, a.Str("name"), a.Get("type"))
			if err != nil {
				return nil, err
			}
			if _, err := client.Delete(ctx, notificationsBase+"/endpoints/"+typ+"/"+enc(a.Get("name")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("name"), "type", typ), nil
		},
	})

	c.Define("proxmox_test_notification_target", Def{
		Toolset:     notificationsToolset,
		Title:       "Send a test notification",
		Description: "Send a test notification to a target to check it is configured correctly (mail delivered, webhook reachable...). Errors from the endpoint are returned.",
		Write:       true,
		Destructive: False,
		Input:       []Prop{P("name", notificationsNameArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Post(ctx, notificationsBase+"/targets/"+enc(a.Get("name"))+"/test", nil); err != nil {
				return nil, err
			}
			return js.O("tested", a.Get("name"), "sent", true), nil
		},
	})

	// ---------------------------------------------------------------- matchers

	c.Define("proxmox_list_notification_matchers", Def{
		Toolset:     notificationsToolset,
		Title:       "List notification matchers",
		Description: "List notification matchers: rules that route notifications (by severity, metadata fields such as type/hostname/job-id, and calendar time) to targets.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			matchers, err := client.GetList(ctx, notificationsBase+"/matchers", nil)
			if err != nil {
				return nil, err
			}
			out := make([]*Obj, len(matchers))
			for i, m := range matchers {
				out[i] = notificationsSummarizeMatcher(m)
			}
			return notificationsSortByName(out), nil
		},
	})

	c.Define("proxmox_get_notification_matcher", Def{
		Toolset:     notificationsToolset,
		Title:       "Get notification matcher",
		Description: "Get one notification matcher's rules and targets (plus its digest for safe updates).",
		Input:       []Prop{P("name", notificationsMatcherNameArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			m, err := client.GetObj(ctx, notificationsBase+"/matchers/"+enc(a.Get("name")), nil)
			if err != nil {
				return nil, err
			}
			return proxmox.Compact(js.Spread(notificationsSummarizeMatcher(js.O("name", a.Get("name")).Assign(m)), js.O("digest", m.Get("digest")))), nil
		},
	})

	c.Define("proxmox_save_notification_matcher", Def{
		Toolset: notificationsToolset,
		Title:   "Create or update a notification matcher",
		Description: "Create (create=true) or update a notification matcher. Given lists replace the existing ones. A matcher with no match rules matches everything. " +
			"match_severity: info, notice, warning, error, unknown. match_field: '(exact|regex):<field>=<value>', e.g. 'exact:type=vzdump' or 'regex:hostname=^pve'; see proxmox_get_notification_matcher_fields for known fields and values. " +
			"match_calendar: time windows like 'mon..fri 8-17'. mode: all (default) or any of the rules must match.",
		Write:       true,
		Destructive: False,
		Input: []Prop{
			P("name", notificationsMatcherNameArg),
			P("create", Bool().Opt().D("Create a new matcher (default false = update)")),
			P("targets", Arr(Str()).Opt().D("Target names to notify on match")),
			P("match_severity", Arr(Enum("info", "notice", "warning", "error", "unknown")).Opt().D("Severities to match")),
			P("match_field", Arr(Str()).Opt().D("Metadata rules, e.g. [\"exact:type=vzdump\", \"regex:hostname=^pve1$\"]")),
			P("match_calendar", Arr(Str()).Opt().D("Calendar windows, e.g. [\"mon..fri 9-17\"]")),
			P("mode", Enum("all", "any").Opt().D("Whether all (default) or any of the rules must match")),
			P("invert_match", Bool().Opt().D("Invert the whole matcher")),
			P("disable", Bool().Opt().D("Disable the matcher")),
			P("comment", Str().Opt().D("Comment")),
			P("delete", notificationsDeleteArrayArg),
			P("digest", digestArg),
			P("extra", extraArg),
		},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			params, err := definedExtra(js.O(
				"target", a.Get("targets"),
				"match-severity", a.Get("match_severity"),
				"match-field", a.Get("match_field"),
				"match-calendar", a.Get("match_calendar"),
				"mode", a.Get("mode"),
				"invert-match", a.Get("invert_match"),
				"disable", a.Get("disable"),
				"comment", a.Get("comment"),
			), a.Get("extra"))
			if err != nil {
				return nil, err
			}
			create := a.Truthy("create")
			path := notificationsBase + "/matchers/" + enc(a.Get("name"))
			if create {
				_, err = client.Post(ctx, notificationsBase+"/matchers", js.Spread(js.O("name", a.Get("name")), params))
			} else {
				_, err = client.Put(ctx, path, js.Spread(params, notificationsDeleteDigest(a)))
			}
			if err != nil {
				return nil, err
			}
			var matcher any = js.Undef
			if m, err := client.Get(ctx, path, nil); err == nil {
				if o, ok := m.(*Obj); ok && o != nil {
					matcher = notificationsSummarizeMatcher(js.O("name", a.Get("name")).Assign(o))
				}
			}
			return proxmox.C(pick(create, "created", "updated").(string), a.Get("name"), "matcher", matcher), nil
		},
	})

	c.Define("proxmox_delete_notification_matcher", Def{
		Toolset:     notificationsToolset,
		Title:       "Delete a notification matcher",
		Description: "Delete a notification matcher. Notifications it routed will no longer reach its targets.",
		Delete:      true,
		Input:       []Prop{P("name", notificationsMatcherNameArg)},
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			if _, err := client.Delete(ctx, notificationsBase+"/matchers/"+enc(a.Get("name")), nil); err != nil {
				return nil, err
			}
			return js.O("deleted", a.Get("name")), nil
		},
	})

	c.Define("proxmox_get_notification_matcher_fields", Def{
		Toolset:     notificationsToolset,
		Title:       "Get notification matcher fields",
		Description: "List the metadata fields notifications carry (type, hostname, job-id...) and their known values, for writing match_field rules in proxmox_save_notification_matcher.",
		Handler: func(ctx context.Context, a *Obj) (any, error) {
			fields, err := client.GetList(ctx, notificationsBase+"/matcher-fields", nil)
			if err != nil {
				return nil, err
			}
			values, err := client.GetList(ctx, notificationsBase+"/matcher-field-values", nil)
			if err != nil {
				values = nil
			}
			out := js.NewObj()
			for _, f := range fields {
				out.Set(js.Str(f.Get("name")), []any{})
			}
			for _, v := range values {
				k := js.Str(v.Get("field"))
				list, _ := out.Get(k).([]any)
				out.Set(k, append(list, proxmox.C("value", v.Get("value"), "comment", v.Get("comment"))))
			}
			keys := out.Keys()
			notificationsSortStrings(keys)
			result := make([]any, len(keys))
			for i, field := range keys {
				vals, _ := out.Get(field).([]any)
				mapped := make([]any, len(vals))
				for j, v := range vals {
					o := v.(*Obj)
					if o.Truthy("comment") {
						mapped[j] = o
					} else {
						mapped[j] = o.Get("value")
					}
				}
				result[i] = proxmox.C("field", field, "values", mapped)
			}
			return js.O("fields", result, "example", "exact:type=vzdump"), nil
		},
	})
}

// notificationsSortStrings sorts strings with localeCompare, stably.
func notificationsSortStrings(keys []string) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && js.LocaleCompare(keys[j], keys[j-1]) < 0; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}
