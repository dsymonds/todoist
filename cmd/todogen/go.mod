module github.com/dsymonds/todoist/cmd/todogen

go 1.24.3

require (
	cloud.google.com/go v0.122.0
	github.com/dsymonds/todoist v0.0.0-20250921082610-0f727c4db1b6
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/google/uuid v1.6.0 // indirect

replace github.com/dsymonds/todoist => ../..
