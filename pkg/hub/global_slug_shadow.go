// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A template or harness config is resolved by name in the agent's project
// first and globally second (resolveTemplateRef, resolveHarnessConfigBySlug,
// and the CLI's own lookup), so a project-scope record whose slug equals a
// global record's replaces the global one for every agent in that project.
// The CLI's lookup (cmd/template_resolution.go, cmd/harness_config.go) lists
// records whose name or slug equals the name typed and takes the first whose
// name or slug matches, so a record's name shadows as well as its slug.
// Project members may create project templates and harness configs, and their
// contents run inside the agents that use them, with the secrets of each
// agent's creator. On a hosted hub a project-scope record may therefore not
// use a global record's slug or name as its own slug or name, as written or
// slugified: create, rename, clone and import refuse it. This follows the
// scope of checkHarnessConfigSlugNotGlobal (harness_config_global_slug.go),
// which refuses a global harness config slug on the same terms, and adds
// names and templates: a user-scope record is resolved only for its own
// user, and a workstation hub has a single user.
//
// Resolution order is unchanged. Project-first is what the CLI, the
// "project:" and "global:" name prefixes and existing projects rely on, and
// global-first in the hub alone would make the hub and the CLI resolve the
// same name differently. A global record created later with the slug of an
// existing project record is a hub administrator's decision and is not
// refused.

// errShadowsGlobalSlug is returned (wrapped in a globalShadowError) when a
// project-scope template or harness config would take the slug or
// name of a global one.
var errShadowsGlobalSlug = errors.New("slug or name is used by a global resource")

// globalShadowError is a shadow check refusal: Field ("slug" or "name") of
// the record, read as Value, is a global record's slug or name.
type globalShadowError struct {
	Kind  string
	Field string
	Value string
}

func (e *globalShadowError) Error() string {
	return fmt.Sprintf("%s %s %q: %v", e.Kind, e.Field, e.Value, errShadowsGlobalSlug)
}

func (e *globalShadowError) Unwrap() error { return errShadowsGlobalSlug }

// shadowCandidate is one value a record could be found by.
type shadowCandidate struct {
	field, value string
}

// shadowCandidates returns the values a record with name and slug could be
// found by: each as stored (a PATCH stores a caller's slug as given) and
// slugified, slug first, without empty or repeated values.
func shadowCandidates(name, slug string) []shadowCandidate {
	var out []shadowCandidate
	seen := map[string]bool{}
	add := func(field, v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, shadowCandidate{field, v})
	}
	add("slug", slug)
	add("slug", api.Slugify(slug))
	add("name", name)
	add("name", api.Slugify(name))
	return out
}

// templateShadowsGlobal reports, as a globalShadowError, whether a template
// of scope with name and slug would shadow a global template: whether any
// of its shadowCandidates is a global template's name or slug (the store's
// Name filter matches either). Only project scope on a hosted hub is
// checked. A lookup failure is returned as is.
func (s *Server) templateShadowsGlobal(ctx context.Context, scope, name, slug string) error {
	if s.workstation || scope != store.TemplateScopeProject {
		return nil
	}
	for _, c := range shadowCandidates(name, slug) {
		found, err := s.store.ListTemplates(ctx, store.TemplateFilter{Name: c.value, Scope: store.TemplateScopeGlobal},
			store.ListOptions{Limit: 1, SkipTotalCount: true})
		if err != nil {
			return err
		}
		if len(found.Items) > 0 {
			return &globalShadowError{Kind: "template", Field: c.field, Value: c.value}
		}
	}
	return nil
}

// harnessConfigShadowsGlobal is templateShadowsGlobal for harness configs.
func (s *Server) harnessConfigShadowsGlobal(ctx context.Context, scope, name, slug string) error {
	if s.workstation || scope != store.HarnessConfigScopeProject {
		return nil
	}
	for _, c := range shadowCandidates(name, slug) {
		found, err := s.store.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Name: c.value, Scope: store.HarnessConfigScopeGlobal},
			store.ListOptions{Limit: 1, SkipTotalCount: true})
		if err != nil {
			return err
		}
		if len(found.Items) > 0 {
			return &globalShadowError{Kind: "harness config", Field: c.field, Value: c.value}
		}
	}
	return nil
}

// writeGlobalSlugShadowError writes the response for a shadow check error:
// 409 for a globalShadowError, the store error otherwise. It reports
// whether err was non-nil (and so a response was written).
func writeGlobalSlugShadowError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var shadow *globalShadowError
	if errors.As(err, &shadow) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("a global %s named %q already exists; a project %s cannot use the same name or slug, because it would replace the global one. Choose a different name.", shadow.Kind, shadow.Value, shadow.Kind),
			map[string]interface{}{"field": shadow.Field, "reason": "shadows_global"})
		return true
	}
	writeErrorFromErr(w, err, "")
	return true
}

// refuseTemplateGlobalShadow checks a template's scope, name and slug and
// writes the response when it must not be stored. It reports whether it
// wrote one.
func (s *Server) refuseTemplateGlobalShadow(w http.ResponseWriter, ctx context.Context, scope, name, slug string) bool {
	return writeGlobalSlugShadowError(w, s.templateShadowsGlobal(ctx, scope, name, slug))
}

// refuseHarnessConfigGlobalShadow is refuseTemplateGlobalShadow for harness
// configs.
func (s *Server) refuseHarnessConfigGlobalShadow(w http.ResponseWriter, ctx context.Context, scope, name, slug string) bool {
	return writeGlobalSlugShadowError(w, s.harnessConfigShadowsGlobal(ctx, scope, name, slug))
}
