package ui

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"butler/internal/store"
)

// mentionToken matches the @query the human is currently typing, at the end
// of the composer. Titles may contain spaces; the query itself is the slug
// characters typed so far, and the server offers titles that contain it.
var mentionToken = regexp.MustCompile(`(?:^|\s)@([A-Za-z0-9_:-]*)$`)

type mentionItem struct {
	Name string
	Vals string
}

type mentionMenu struct {
	SessionID string
	Items     []mentionItem
}

// mentionTargets are the @completions for a room: every other participant's
// display title (the id only when the role has no title) plus "all". The
// human is excluded. This is the same set the old composer menu offered.
func mentionTargets(parts []store.Agent) []string {
	out := []string{"all"}
	seen := map[string]bool{"all": true}
	for _, pa := range parts {
		if pa.ID == AdminAgentID {
			continue
		}
		label := pa.DisplayName
		if label == "" {
			label = pa.ID
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	return out
}

func trailingMention(content string) (string, bool) {
	m := mentionToken.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func applyMention(content, name string) string {
	loc := mentionToken.FindStringSubmatchIndex(content)
	if loc == nil {
		if content != "" && !strings.HasSuffix(content, " ") {
			content += " "
		}
		return content + "@" + name + " "
	}
	at := loc[2] - 1
	return content[:at] + "@" + name + " " + content[loc[1]:]
}

func (p *Panel) mentionSuggest(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	query, ok := trailingMention(r.PostFormValue("content"))
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	parts, err := p.Store.Participants(r.Context(), sid)
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	q := strings.ToLower(query)
	var items []mentionItem
	for _, name := range mentionTargets(parts) {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		raw, err := json.Marshal(map[string]string{"mention": name})
		if err != nil {
			continue
		}
		items = append(items, mentionItem{Name: name, Vals: string(raw)})
		if len(items) == 8 {
			break
		}
	}
	p.renderFragment(w, "mention-menu", mentionMenu{SessionID: sid, Items: items})
}

func (p *Panel) mentionApply(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("mention"))
	parts, err := p.Store.Participants(r.Context(), sid)
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	allowed := false
	for _, n := range mentionTargets(parts) {
		if n == name {
			allowed = true
			break
		}
	}
	if !allowed || name == "" {
		http.Error(w, "unknown mention", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = p.templates.ExecuteTemplate(w, "composer", composerView{
		SessionID: sid,
		Value:     applyMention(r.PostFormValue("content"), name),
		OOB:       true,
		Focus:     true,
	})
	_ = p.templates.ExecuteTemplate(w, "mention-clear", nil)
}
