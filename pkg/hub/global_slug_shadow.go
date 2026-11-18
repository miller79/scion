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
// Project members may create project templates and harness configs, and their
// contents run inside the agents that use them, with the secrets of each
// agent's creator. A project- or user-scope record may therefore not take the
// slug of a global record: create, rename, clone and import refuse it.
//
// Resolution order is unchanged. Project-first is what the CLI, the
// "project:" and "global:" name prefixes and existing projects rely on, and
// global-first in the hub alone would make the hub and the CLI resolve the
// same name differently. A global record created later with the slug of an
// existing project record is a hub administrator's decision and is not
// refused.

// errShadowsGlobalSlug is returned when a project- or user-scope template or
// harness config would take the slug of a global one.
var errShadowsGlobalSlug = errors.New("slug is used by a global resource")

// slugShadowCandidates returns the slugs to check for slug: the value as
// stored and its normalized form (a PATCH stores a caller's slug as given).
func slugShadowCandidates(slug string) []string {
	out := []string{slug}
	if n := api.Slugify(slug); n != "" && n != slug {
		out = append(out, n)
	}
	return out
}

// templateSlugShadowsGlobal reports, as errShadowsGlobalSlug, whether a
// template of scope with slug would shadow a global template. Global scope
// and an empty slug never do. A lookup failure other than not-found is
// returned as is.
func (s *Server) templateSlugShadowsGlobal(ctx context.Context, scope, slug string) error {
	if slug == "" || scope == "" || scope == store.TemplateScopeGlobal {
		return nil
	}
	for _, candidate := range slugShadowCandidates(slug) {
		existing, err := s.store.GetTemplateBySlug(ctx, candidate, store.TemplateScopeGlobal, "")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if existing != nil {
			return fmt.Errorf("template %q: %w", candidate, errShadowsGlobalSlug)
		}
	}
	return nil
}

// harnessConfigSlugShadowsGlobal is templateSlugShadowsGlobal for harness
// configs.
func (s *Server) harnessConfigSlugShadowsGlobal(ctx context.Context, scope, slug string) error {
	if slug == "" || scope == "" || scope == store.HarnessConfigScopeGlobal {
		return nil
	}
	for _, candidate := range slugShadowCandidates(slug) {
		existing, err := s.store.GetHarnessConfigBySlug(ctx, candidate, store.HarnessConfigScopeGlobal, "")
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if existing != nil {
			return fmt.Errorf("harness config %q: %w", candidate, errShadowsGlobalSlug)
		}
	}
	return nil
}

// writeGlobalSlugShadowError writes the response for a shadow check error:
// 409 for errShadowsGlobalSlug, the store error otherwise. It reports
// whether err was non-nil (and so a response was written).
func writeGlobalSlugShadowError(w http.ResponseWriter, kind, slug string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errShadowsGlobalSlug) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			fmt.Sprintf("a global %s named %q already exists; a project or user %s cannot use the same name, because it would replace the global one. Choose a different name.", kind, slug, kind),
			map[string]interface{}{"field": "slug", "reason": "shadows_global"})
		return true
	}
	writeErrorFromErr(w, err, "")
	return true
}

// refuseTemplateGlobalShadow checks a template's scope and slug and writes
// the response when it must not be stored. It reports whether it wrote one.
func (s *Server) refuseTemplateGlobalShadow(w http.ResponseWriter, ctx context.Context, scope, slug string) bool {
	return writeGlobalSlugShadowError(w, "template", slug, s.templateSlugShadowsGlobal(ctx, scope, slug))
}

// refuseHarnessConfigGlobalShadow is refuseTemplateGlobalShadow for harness
// configs.
func (s *Server) refuseHarnessConfigGlobalShadow(w http.ResponseWriter, ctx context.Context, scope, slug string) bool {
	return writeGlobalSlugShadowError(w, "harness config", slug, s.harnessConfigSlugShadowsGlobal(ctx, scope, slug))
}
