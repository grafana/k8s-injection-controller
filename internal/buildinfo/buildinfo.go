/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package buildinfo exposes version metadata that is stamped into the binary at
// build time via -ldflags. When built without those flags (e.g. `go run`), the
// values fall back to "dev"/"unknown" so the controller still starts cleanly.
package buildinfo

// These variables are set at build time with:
//
//	-ldflags "-X github.com/grafana/beyla-k8s-injector/internal/buildinfo.Version=v1.2.3 ..."
//
// See the Makefile (`make build`, `make docker-build`) and the Dockerfile for
// the canonical set of ldflags.
var (
	// Version is the semantic version of the release (e.g. "v1.2.3").
	Version = "dev"
	// Revision is the git commit the binary was built from.
	Revision = "unknown"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)
