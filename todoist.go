/*
Package todoist contains tools for interacting with the Todoist API.

The Syncer is likely the place to start for interacting with the API.
*/
package todoist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// See https://developer.todoist.com/sync/v9/ for the reference for types and protocols.
// https://developer.todoist.com/rest/v2/ is also used for several operations.

// Project represents a Todoist project.
type Project struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

// Collaborator represents a Todoist collaborator.
type Collaborator struct {
	ID string `json:"id"`

	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// Item represents a Todoist item (task).
type Item struct {
	ID          string   `json:"id,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	Content     string   `json:"content,omitempty"`     // title of task
	Description string   `json:"description,omitempty"` // secondary info
	Priority    int      `json:"priority,omitempty"`    // 4 is the highest priority, 1 is the lowest
	Labels      []string `json:"labels,omitempty"`

	Responsible *string `json:"responsible_uid,omitempty"`
	Checked     bool    `json:"checked,omitempty"`
	Due         *Due    `json:"due,omitempty"`

	ParentID       string `json:"parent_id,omitempty"`
	ChildRemaining int    `json:"-"`
	ChildCompleted int    `json:"-"`
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
	type compInfo struct {
		// One of these should be set:
		ProjectID string `json:"project_id"`
		SectionID string `json:"section_id"`
		ItemID    string `json:"item_id"`

		NumItems int `json:"completed_items"`
	}
	var data struct {
		SyncToken     string         `json:"sync_token"`
		FullSync      bool           `json:"full_sync"`
		Projects      []Project      `json:"projects"`
		Collaborators []Collaborator `json:"collaborators"`
		Items         []Item         `json:"items"`
		Completed     []compInfo     `json:"completed_info"`
	}
	err := ts.postForm(ctx, "/sync/v9/sync", url.Values{
		"sync_token": []string{ts.syncToken},
		// TODO: sync more, and permit configuring what things to sync.
		"resource_types": []string{`["projects","items","collaborators","completed_info"]`},
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
	for _, comp := range data.Completed {
		if comp.ItemID == "" { // only handle completion counts for items
			continue
		}
		item, ok := ts.Items[comp.ItemID]
		if !ok {
			// This really shouldn't happen.
			// TODO: Or can it happen for completed items?
			log.Printf("WARNING: Todoist reported completion info for unknown item %s", comp.ItemID)
			continue
		}
		item.ChildCompleted = comp.NumItems
		ts.Items[comp.ItemID] = item
	}
	ts.syncToken = data.SyncToken

	// Recompute pending children.
	for id, item := range ts.Items {
		item.ChildRemaining = 0
		ts.Items[id] = item
	}
	for _, item := range ts.Items {
		if item.ParentID == "" { // TODO: skip checked if we start tracking them
			continue
		}
		p, ok := ts.Items[item.ParentID]
		if !ok {
			// This really shouldn't happen.
			// TODO: Or can it happen for completed items?
			log.Printf("WARNING: Todoist item %q has parent %s that we don't know about", item.Content, item.ParentID)
			continue
		}
		p.ChildRemaining++
		ts.Items[item.ParentID] = p
	}

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
		vs.Set("due_datetime", item.Due.Date)
	}
	if item.Responsible != nil {
		vs.Set("assignee_id", *item.Responsible)
	}
	err := ts.postForm(ctx, "/rest/v2/tasks", vs, &struct{}{})
	return err
}

func (ts *Syncer) postForm(ctx context.Context, path string, params url.Values, dst interface{}) error {
	return ts.post(ctx, path, strings.NewReader(params.Encode()), "application/x-www-form-urlencoded", dst)
}

func (ts *Syncer) postJSON(ctx context.Context, path string, reqBody, dst interface{}) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling JSON body: %w", err)
	}
	return ts.post(ctx, path, bytes.NewReader(b), "application/json", dst)
}

func (ts *Syncer) post(ctx context.Context, path string, reqBody io.Reader, ct string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.todoist.com"+path, reqBody)
	if err != nil {
		return fmt.Errorf("constructing HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.apiToken)
	req.Header.Set("Content-Type", ct)

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

func (ts *Syncer) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", "https://api.todoist.com"+path, nil)
	if err != nil {
		return fmt.Errorf("constructing HTTP request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ts.apiToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_, err = ioutil.ReadAll(resp.Body) // TODO: report/log this if there's a failure
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("reading API response body: %w", err)
	}
	if resp.StatusCode != 204 {
		return fmt.Errorf("API request returned %s", resp.Status)
	}
	return nil
}

// ProjectByName returns the named project.
// This will only work after a Sync invocation.
func (s *Syncer) ProjectByName(name string) (Project, bool) {
	for _, proj := range s.Projects {
		if proj.Name == name {
			return proj, true
		}
	}
	return Project{}, false
}

// Assign assigns a task to the given UID.
// If it is the empty string, the item is unassigned.
func (s *Syncer) Assign(ctx context.Context, item Item, assignee string) error {
	var req struct {
		AssigneeID *string `json:"assignee_id"`
	}
	if assignee != "" {
		req.AssigneeID = &assignee
	}
	return s.postJSON(ctx, "/rest/v2/tasks/"+url.PathEscape(item.ID), req, &struct{}{})
}

type ItemUpdates struct {
	Content *string   `json:"content,omitempty"`
	Labels  *[]string `json:"labels,omitempty"`
}

// UpdateItem updates an item.
func (s *Syncer) UpdateItem(ctx context.Context, item Item, updates ItemUpdates) error {
	// TODO: refresh the sync state?

	return s.postJSON(ctx, "/rest/v2/tasks/"+url.PathEscape(item.ID), updates, &struct{}{})
}

func (s *Syncer) DeleteItem(ctx context.Context, item Item) error {
	return s.delete(ctx, "/rest/v2/tasks/"+url.PathEscape(item.ID))
}
