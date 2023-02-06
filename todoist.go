/*
Package todoist contains tools for interacting with the Todoist API.

The Syncer is likely the place to start for interacting with the API.
*/
package todoist

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// See https://developer.todoist.com/sync/v9/ for the reference for types and protocols.

// Project represents a Todoist project.
type Project struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

// Collaborator represents a Todoist collaborator.
type Collaborator struct {
	ID string `json:"id"`

	FullName string `json:"full_name"`

	// TODO: email
}

// Item represents a Todoist item (task).
type Item struct {
	ID          string `json:"id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Content     string `json:"content,omitempty"`     // title of task
	Description string `json:"description,omitempty"` // secondary info
	Priority    int    `json:"priority,omitempty"`

	Responsible *string `json:"responsible_uid,omitempty"`
	Checked     bool    `json:"checked,omitempty"`
	Due         *Due    `json:"due,omitempty"`
}

// Due represents a task's due date, in a few different possible formats.
type Due struct {
	Date string `json:"date"` // YYYY-MM-DD or YYYY-MM-DDTHH:MM:SS or YYYY-MM-DDTHH:MM:SSZ

	// Parsed from Date on sync.
	y       int
	m       time.Month
	d       int
	hasTime bool
	hh, mm  int // only if hasTime
	due     time.Time

	IsRecurring bool `json:"is_recurring"`
}

// Time reports the due date's exact time, if a time is associated with it.
func (dd *Due) Time() (time.Time, bool) {
	if !dd.hasTime {
		return time.Time{}, false
	}
	return dd.due, true
}

// When reports when the due date is relative to today.
// This is -1, 0 or 1 for overdue, today and future due dates.
func (dd *Due) When() int {
	now := time.Now()
	// Cheapest check based solely on the date.
	y, m, d := now.Date()
	if dd.y != y {
		return cmp(dd.y, y)
	} else if dd.m != m {
		return cmp(int(dd.m), int(m))
	} else if dd.d != d {
		return cmp(dd.d, d)
	}
	// Remaining check is for things due today.
	if dd.due.Before(now) {
		return -1
	}
	return 0
}

func cmp(x, y int) int {
	if x < y {
		return -1
	}
	return 1
}

func (dd *Due) update() error {
	if !strings.Contains(dd.Date, "T") {
		// YYYY-MM-DD (full-day date)
		t, err := time.ParseInLocation("2006-01-02", dd.Date, time.Local)
		if err != nil {
			return fmt.Errorf("parsing full-day date %q: %w", dd.Date, err)
		}
		dd.y, dd.m, dd.d = t.Date()
		dd.due = time.Date(dd.y, dd.m, dd.d, 23, 59, 59, 0, time.Local)
		return nil
	}
	// YYYY-MM-DDTHH:MM:SS or YYYY-MM-DDTHH:MM:SSZ
	str, loc := dd.Date, time.Local
	if strings.HasSuffix(str, "Z") {
		str = str[:len(str)-1]
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", str, loc)
	if err != nil {
		return fmt.Errorf("parsing due date with time %q: %w", dd.Date, err)
	}
	dd.y, dd.m, dd.d = t.Date()
	dd.hasTime = true
	dd.hh, dd.mm, _ = t.Clock()
	dd.due = t
	return nil
}

type Syncer struct {
	apiToken string

	// State. Maps are keyed by the relevant ID.
	syncToken     string
	Projects      map[string]Project
	Collaborators map[string]Collaborator
	Items         map[string]Item // Only incomplete (TODO: relax this)
}

func NewSyncer(apiToken string) *Syncer {
	return &Syncer{
		apiToken:  apiToken,
		syncToken: "*", // this means next sync should get all data
	}
}

// Sync triggers a synchronisation of data, doing a partial sync where possible.
func (ts *Syncer) Sync(ctx context.Context) error {
	var data struct {
		SyncToken     string         `json:"sync_token"`
		FullSync      bool           `json:"full_sync"`
		Projects      []Project      `json:"projects"`
		Collaborators []Collaborator `json:"collaborators"`
		Items         []Item         `json:"items"`
	}
	err := ts.post(ctx, "/sync/v9/sync", url.Values{
		"sync_token": []string{ts.syncToken},
		// TODO: sync more, and permit configuring what things to sync.
		"resource_types": []string{`["projects","items","collaborators"]`},
	}, &data)
	if err != nil {
		return err
	}

	if data.FullSync || ts.Projects == nil {
		// Server says this is a full sync, or this is the first sync we've attempted.
		ts.Projects = make(map[string]Project)
		ts.Collaborators = make(map[string]Collaborator)
		ts.Items = make(map[string]Item)
	}
	for _, p := range data.Projects {
		// TODO: Handle deletions. This is pretty uncommon.
		ts.Projects[p.ID] = p
	}
	for _, c := range data.Collaborators {
		// TODO: Handle deletions. It's uncommon.
		ts.Collaborators[c.ID] = c
	}
	for _, item := range data.Items {
		if item.Checked {
			delete(ts.Items, item.ID)
		} else {
			if item.Due != nil {
				item.Due.update()
			}
			ts.Items[item.ID] = item
		}
	}
	ts.syncToken = data.SyncToken
	return nil
}

// CreateItem creates an item, without going through the full sync workflow.
func (ts *Syncer) CreateItem(ctx context.Context, item Item) error {
	vs := url.Values{
		"content":    []string{item.Content},
		"project_id": []string{item.ProjectID},
		"priority":   []string{strconv.Itoa(item.Priority)},
	}
	if item.Description != "" {
		vs.Set("description", item.Description)
	}
	if item.Due != nil {
		vs.Set("date_string", item.Due.Date)
	}
	err := ts.post(ctx, "/sync/v9/items/add", vs, &struct{}{})
	return err
}

func (ts *Syncer) post(ctx context.Context, path string, params url.Values, dst interface{}) error {
	form := strings.NewReader(params.Encode())
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.todoist.com"+path, form)
	if err != nil {
		return fmt.Errorf("constructing HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.apiToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading API response body: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("API request returned %s", resp.Status)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parsing API response: %w", err)
	}
	return nil
}
