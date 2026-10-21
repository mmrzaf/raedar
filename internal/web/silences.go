package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmrzaf/raedar/internal/config"
	"github.com/mmrzaf/raedar/internal/store"
)

// maxSilenceDuration bounds a UI-created silence so a typo cannot mute a
// subject for a year.
const maxSilenceDuration = 30 * 24 * time.Hour

// silenceView is one row of the silences page.
type silenceView struct {
	ID        int64
	Kind      string
	Scope     string
	Reason    string
	EndsAt    time.Time
	Active    bool
	Remaining time.Duration
}

// silencesPage renders the silences page and its form.
func (s *Server) silencesPage(w http.ResponseWriter, request *http.Request) {
	s.renderSilences(w, request, http.StatusOK, "")
}

func (s *Server) renderSilences(w http.ResponseWriter, request *http.Request, status int, message string) {
	silences, err := s.store.ListSilences(request.Context(), 100)
	if err != nil {
		s.logger.Error("read silences", "error", err)
		http.Error(w, "cannot read silences", http.StatusInternalServerError)
		return
	}
	now := s.clock.Now()
	views := make([]silenceView, 0, len(silences))
	for _, silence := range silences {
		views = append(views, s.silenceView(silence, now))
	}
	w.WriteHeader(status)
	s.render(w, "silences.html", map[string]any{
		"Page":     pageData{Title: "Silences", HostName: s.cfg.HostName, Now: now, Version: s.version, Sources: s.sourceViews(request), Nav: "silences", Location: s.cfg.Location},
		"Silences": views,
		"Error":    message,
	})
}

// silenceView renders one silence for the page.
func (s *Server) silenceView(silence store.Silence, now time.Time) silenceView {
	scope := "everything"
	parts := make([]string, 0, 2)
	if silence.Rule != nil {
		parts = append(parts, "rule "+*silence.Rule)
	}
	if silence.Subject != nil {
		parts = append(parts, "subject "+s.cfg.SubjectNames.Display(*silence.Subject))
	}
	if silence.AppID != nil {
		name := s.appName(s.model.Model(), *silence.AppID)
		if name == "" {
			name = fmt.Sprintf("#%d", *silence.AppID)
		}
		parts = append(parts, "app "+name)
	}
	if len(parts) > 0 {
		scope = strings.Join(parts, ", ")
	}
	return silenceView{
		ID:        silence.ID,
		Kind:      string(silence.Kind),
		Scope:     scope,
		Reason:    silence.Reason,
		EndsAt:    silence.EndsAt,
		Active:    silence.EndsAt.After(now),
		Remaining: silence.EndsAt.Sub(now),
	}
}

// createSilence handles the silences form. It creates a maintenance
// window, which is the same silence the operator could add from
// Telegram as a mute.
func (s *Server) createSilence(w http.ResponseWriter, request *http.Request) {
	if !sameOrigin(request) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if err := request.ParseForm(); err != nil {
		s.renderSilences(w, request, http.StatusBadRequest, "cannot read the form")
		return
	}
	reason := strings.TrimSpace(request.PostForm.Get("reason"))
	rule := strings.TrimSpace(request.PostForm.Get("rule"))
	subject := strings.TrimSpace(request.PostForm.Get("subject"))
	rawDuration := strings.TrimSpace(request.PostForm.Get("duration"))

	if reason == "" {
		s.renderSilences(w, request, http.StatusBadRequest, "a reason is required")
		return
	}
	duration, err := config.ParseDuration(rawDuration)
	if err != nil {
		s.renderSilences(w, request, http.StatusBadRequest, "duration: "+err.Error())
		return
	}
	if duration > maxSilenceDuration {
		s.renderSilences(w, request, http.StatusBadRequest,
			fmt.Sprintf("duration must be at most %s", maxSilenceDuration))
		return
	}
	now := s.clock.Now()
	silence := store.Silence{
		Kind:      store.SilenceMaintenanceWindow,
		Reason:    reason,
		StartsAt:  now,
		EndsAt:    now.Add(duration),
		CreatedBy: strPtr("ui"),
	}
	if rule != "" {
		silence.Rule = &rule
	}
	if subject != "" {
		silence.Subject = &subject
	}
	if _, err := s.store.CreateSilence(request.Context(), silence); err != nil {
		s.logger.Error("create silence", "error", err)
		s.renderSilences(w, request, http.StatusInternalServerError, "could not save the silence")
		return
	}
	http.Redirect(w, request, "/silences", http.StatusSeeOther)
}

// deleteSilence removes one silence and redirects back to the list.
func (s *Server) deleteSilence(w http.ResponseWriter, request *http.Request) {
	if !sameOrigin(request) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(request.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid silence id", http.StatusBadRequest)
		return
	}
	if _, err := s.store.DeleteSilence(request.Context(), id); err != nil {
		s.logger.Error("delete silence", "error", err, "silence", id)
		http.Error(w, "cannot delete the silence", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, request, "/silences", http.StatusSeeOther)
}

// deliveriesPage renders the recent deliveries.
func (s *Server) deliveriesPage(w http.ResponseWriter, request *http.Request) {
	rows, err := s.store.RecentDeliveries(request.Context(), 200)
	if err != nil {
		s.logger.Error("read deliveries", "error", err)
		http.Error(w, "cannot read deliveries", http.StatusInternalServerError)
		return
	}
	s.render(w, "deliveries.html", map[string]any{
		"Page": pageData{Title: "Deliveries", HostName: s.cfg.HostName, Now: s.clock.Now(), Version: s.version, Sources: s.sourceViews(request), Nav: "deliveries", Location: s.cfg.Location},
		"Rows": rows,
	})
}

// sameOrigin reports whether a form POST comes from this host. It refuses
// absent and foreign origins so a cross-site form cannot mutate state.
func sameOrigin(request *http.Request) bool {
	source := request.Header.Get("Origin")
	if source == "" {
		source = request.Header.Get("Referer")
	}
	if source == "" {
		return false
	}
	parsed, err := url.Parse(source)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, request.Host)
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }
