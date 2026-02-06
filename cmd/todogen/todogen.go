/*
todogen generates Todoist tasks from configuration.
*/
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"log"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/dsymonds/todoist"
	"gopkg.in/yaml.v3"
)

var (
	configFile = flag.String("config", "todogen.yaml", "configuration `file`")
	dryRun     = flag.Bool("n", false, "whether to stop short of creating tasks")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: todogen [options] <program> <yyyy-mm-dd>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}

	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Loading config: %v", err)
	}
	loc := time.UTC
	if cfg.Location != "" {
		l, err := time.LoadLocation(cfg.Location)
		if err != nil {
			log.Fatalf("Loading location: %v", err)
		}
		loc = l
	}

	prog, ok := cfg.Programs[flag.Arg(0)]
	if !ok {
		log.Fatalf("No program %q in config", flag.Arg(0))
	}
	var d0 due
	d0.d, err = civil.ParseDate(flag.Arg(1))
	if err != nil {
		log.Fatalf("Bad date: %v", err)
	}

	ctx := context.Background()
	ts := todoist.NewSyncer(cfg.APIToken)
	log.Printf("Fetching Todoist state...")
	if err := ts.Sync(ctx); err != nil {
		log.Fatalf("Getting Todoist state: %v", err)
	}

	projs := make(map[string]string) // name => ID
	for id, proj := range ts.Projects {
		projs[proj.Name] = id
	}

	var toAdd []todoist.Task
	for _, task := range prog.Tasks {
		projName := prog.Project
		if task.Project != "" {
			projName = task.Project
		}
		projID, ok := projs[projName]
		if !ok {
			log.Fatalf("Didn't find a Todoist project named %q", projName)
		}

		ttask := todoist.Task{
			ProjectID: projID,
			// Content, Description are templated
			Priority: task.Priority,
			Labels:   task.Labels,
		}

		content, err := EvalTextTemplate(task.Content, d0.d, loc)
		if err != nil {
			log.Fatalf("Evaluating content: %v", err)
		}
		ttask.Content = content
		desc, err := EvalTextTemplate(task.Description, d0.d, loc)
		if err != nil {
			log.Fatalf("Evaluating description: %v", err)
		}
		ttask.Description = desc

		if task.Responsible != "" {
			resp, ok := ts.CollaboratorByEmail(task.Responsible)
			if !ok {
				log.Fatalf("Didn't find a collaborator with email %q", task.Responsible)
			}
			ttask.Responsible = &resp.ID
		}

		d := d0
		for i, dm := range task.Due {
			if err := d.apply(dm); err != nil {
				log.Fatalf("Applying due[%d] for %q: %v", i, task.Content, err)
			}
		}
		ttask.Due = &todoist.Due{Date: d.String()}

		toAdd = append(toAdd, ttask)
	}

	var n int
	for _, t := range toAdd {
		if taskExists(ts, t) {
			log.Printf("Task %q already exists; skipping...", t.Content)
			continue
		}
		if *dryRun {
			log.Printf("Would create this task: %+v", t)
			continue
		}
		log.Printf("Adding task %q...", t.Content)
		if err := ts.CreateTask(ctx, t); err != nil {
			log.Fatalf("CreateTask: %v", err)
		}
		n++
	}
	log.Printf("Created %d new tasks", n)
}

func loadConfig(filename string) (Config, error) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("reading config file %s: %v", filename, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config from %s: %v", filename, err)
	}
	return cfg, nil
}

type Config struct {
	APIToken string `yaml:"api_token"`
	Location string `yaml:"location"` // optional; defaults to UTC

	// TODO: aliases for responsible?

	Programs map[string]Program `yaml:"programs"`
}

type Program struct {
	Project string `yaml:"project"`
	Tasks   []Task `yaml:"tasks"`
}

type Task struct {
	Project     string   `yaml:"project"` // if different to the containing program
	Content     string   `yaml:"content"`
	Description string   `yaml:"description"` // optional
	Priority    int      `yaml:"priority"`    // 1=lowest, 4=highest
	Labels      []string `yaml:"labels"`      // optional
	Responsible string   `yaml:"responsible"` // email address; optional
	Due         []DueMod `yaml:"due"`
}

type DueMod struct {
	RelDays     *int         `yaml:"relDays"`
	T           *string      `yaml:"t"`
	PrevWeekday *yamlWeekday `yaml:"prevWeekday"`
}

type due struct {
	d civil.Date
	t *civil.Time // may be nil
}

func (d *due) apply(dm DueMod) error {
	if dm.RelDays != nil {
		d.d = d.d.AddDays(*dm.RelDays)
	}
	if dm.T != nil {
		if d.t != nil {
			return fmt.Errorf("`t` on due date with a time already set")
		}
		t, err := civil.ParseTime(*dm.T)
		if err != nil {
			return fmt.Errorf("parsing `t`: %w", err)
		}
		d.t = &t
	}
	if dm.PrevWeekday != nil {
		delta := int(d.d.Weekday() - time.Weekday(*dm.PrevWeekday))
		if delta < 0 {
			delta += 7
		}
		d.d = d.d.AddDays(-delta)
	}

	return nil
}

// String constructs a due date string in a format acceptable to Todoist.
func (d due) String() string {
	s := d.d.String()
	if d.t != nil {
		s += "T" + d.t.String()
	}
	return s
}

func EvalTextTemplate(tmpl string, d0 civil.Date, loc *time.Location) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	data := struct {
		T0 time.Time
	}{
		T0: time.Date(d0.Year, d0.Month, d0.Day, 0, 0, 0, 0, loc),
	}

	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return sb.String(), nil
}

type yamlWeekday time.Weekday

func (w *yamlWeekday) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	err := unmarshal(&s)
	if err != nil {
		return err
	}
	wd, ok := weekdays[s]
	if !ok {
		return fmt.Errorf("unknown weekday %q", s)
	}
	*w = yamlWeekday(wd)
	return nil
}

var weekdays = make(map[string]time.Weekday)

func init() {
	for d := time.Weekday(0); d < 7; d++ {
		weekdays[d.String()] = d
	}
}

func taskExists(ts *todoist.Syncer, t todoist.Task) bool {
	// Match on project, due date and content.
	for _, et := range ts.Tasks {
		if et.Due == nil {
			continue
		}
		if et.ProjectID == t.ProjectID && et.Due.Date == t.Due.Date && et.Content == t.Content {
			return true
		}
	}
	return false
}
