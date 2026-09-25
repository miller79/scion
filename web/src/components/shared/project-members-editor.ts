/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Project Members Editor — RoleBinding-backed (PM1)
 *
 * Manages project membership via the project-scoped members API:
 *  - Adding a member = POST /api/v1/projects/{id}/members
 *  - Changing a member's role = PATCH /api/v1/projects/{id}/members/{bindingID}
 *  - Removing a member = DELETE /api/v1/projects/{id}/members/{bindingID}
 *  - Assigning a custom project role = POST /api/v1/admin/role-bindings
 *    (owners only; the members API accepts built-in roles only)
 *  - Shows provenance (direct vs. group-derived)
 *  - Owner protection: prevents removing the last direct owner
 *
 * The server returns enriched bindings with roleName and source fields,
 * and the PATCH endpoint performs atomic role changes.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import type { MembershipCapabilities } from '../../shared/types.js';
import type { PrincipalChangeDetail } from './principal-picker.js';
import { showConfirm } from './confirm-dialog.js';
import './principal-picker.js';
import {
  BUILT_IN_PROJECT_MEMBERSHIP_ROLES,
  PROJECT_DIRECT_USER_ONLY_ROLES,
  PROJECT_OWNER_ROLE_NAMES,
  getPrincipalIcon,
  getRoleTier,
} from './role-binding-utils.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface ProjectMemberBinding {
  id: string;
  roleDefinitionId: string;
  roleName: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
  /** 'direct' or the group name this was inherited through. */
  source: 'direct' | string;
  sourceGroupName?: string;
}

interface ProjectRole {
  id: string;
  name: string;
  scopeType: string;
}

/** One table row: a principal's membership binding plus any custom-role
 *  bindings it holds in this project, from the same source. */
export interface MemberRow {
  key: string;
  /** The built-in membership binding, or null for custom-role-only rows. */
  primary: ProjectMemberBinding | null;
  custom: ProjectMemberBinding[];
  principalType: string;
  principalId: string;
  displayName: string;
  source: string;
  sourceGroupName?: string | undefined;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-project-members-editor')
export class ScionProjectMembersEditor extends LitElement {
  /** The project ID. */
  @property() projectId = '';

  /** Whether the editor is read-only. */
  @property({ type: Boolean }) readOnly = false;

  /** Whether to render in compact card layout. */
  @property({ type: Boolean }) compact = false;

  /** Section title override. */
  @property() sectionTitle = 'Members';

  /** Section description override. */
  @property() sectionDescription = '';

  @state() private loading = true;
  @state() private members: ProjectMemberBinding[] = [];
  @state() private projectRoles: ProjectRole[] = [];
  /** Custom project-scoped roles. These add to a principal's membership role
   *  rather than replacing it, so they are edited as checkboxes beside it. */
  @state() private customRoles: ProjectRole[] = [];
  @state() private error: string | null = null;

  // Add dialog state
  @state() private addDialogOpen = false;
  @state() private addPrincipalType = 'user';
  @state() private addPrincipalId = '';
  @state() private addRoleId = '';
  @state() private addLoading = false;
  @state() private addError: string | null = null;

  // Edit member dialog state
  @state() private changeDialogOpen = false;
  @state() private changeRow: MemberRow | null = null;
  @state() private changeRoleId = '';
  /** Custom role definition IDs checked in the edit dialog. */
  @state() private changeCustomIds: string[] = [];
  @state() private changeLoading = false;
  @state() private changeError: string | null = null;

  // Remove state
  @state() private removingMemberId: string | null = null;

  // Membership capabilities returned by the server — drives per-row
  // visibility of edit/remove buttons and the transfer ownership UI.
  @state() private capabilities: MembershipCapabilities | null = null;

  // Transfer ownership dialog state
  @state() private transferDialogOpen = false;
  @state() private transferNewOwnerId = '';
  @state() private transferLoading = false;
  @state() private transferError: string | null = null;

  // Action feedback
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
      gap: 1rem;
    }

    .section-header-info h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .section-header-actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      flex-shrink: 0;
    }

    .member-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 400;
      margin-left: 0.5rem;
    }

    /* Table */
    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .compact .table-container {
      border: none;
      border-radius: 0;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    tr:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    /* Member identity */
    .member-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .member-icon {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .member-icon.user {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .member-icon.group {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-600, #d97706);
    }

    .member-icon.agent {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .member-icon sl-icon {
      font-size: 0.875rem;
    }

    .member-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .member-name {
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .member-detail {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Role badge */
    .role-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .role-badges {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
    }

    .role-badge.custom {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    /* Edit dialog */
    .dialog-member {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }

    .form-help {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0.375rem 0 0 0;
    }

    .custom-role-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      margin-top: 0.5rem;
    }

    .form-label {
      display: block;
      font-size: var(--sl-input-label-font-size-medium, 0.875rem);
      margin-bottom: 0.25rem;
    }

    /* Provenance badge */
    .provenance-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.6875rem;
    }

    .provenance-badge.direct {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .provenance-badge.group-derived {
      color: var(--sl-color-warning-600, #d97706);
    }

    .provenance-badge sl-icon {
      font-size: 0.6875rem;
    }

    .actions-cell {
      text-align: right;
      white-space: nowrap;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty state */
    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
    }

    .compact .empty-state {
      padding: 2rem 1.5rem;
    }

    .empty-state > sl-icon {
      font-size: 3rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 0.75rem;
    }

    .empty-state h3 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
      font-size: 0.875rem;
    }

    /* Loading / Error */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    /* Dialog */
    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .validation-warning {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-warning-700, #b45309);
      font-size: 0.8125rem;
    }

    .validation-warning sl-icon {
      flex-shrink: 0;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .section {
        padding: 1rem;
      }

      th,
      td {
        padding: 0.5rem 0.75rem;
      }
    }
  `;

  /** Guard to prevent double-fetch when connectedCallback and updated both fire. */
  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      this._initialLoadDone = true;
      void this.loadData();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      // Skip if connectedCallback already triggered the initial load.
      if (!this._initialLoadDone) {
        void this.loadData();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    if (!this.projectId) return;

    this.loading = true;
    this.error = null;

    try {
      // PM1: Use project-scoped members endpoint + roles list in parallel.
      const [membersRes, rolesRes] = await Promise.all([
        apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}/members`),
        apiFetch('/api/v1/admin/roles'),
      ]);

      if (!membersRes.ok) {
        throw new Error(await extractApiError(membersRes, `HTTP ${membersRes.status}`));
      }

      const data = (await membersRes.json()) as {
        items?: ProjectMemberBinding[];
        _capabilities?: {
          canManageMembers?: boolean;
          canManageAdmins?: boolean;
          canManageOwners?: boolean;
          canTransfer?: boolean;
          actions?: string[];
        };
      };
      // Server returns enriched items with roleName and source.
      this.members = (data.items || []).map((b) => ({
        ...b,
        source: b.source || 'direct',
      }));

      // Parse membership capabilities from the server response.
      // When _capabilities is absent (older server), default to null
      // which makes effectiveReadOnly true (fail-closed).
      const rawCaps = data._capabilities;
      if (rawCaps) {
        this.capabilities = {
          canManageMembers: rawCaps.canManageMembers ?? false,
          canManageAdmins: rawCaps.canManageAdmins ?? false,
          canManageOwners: rawCaps.canManageOwners ?? false,
          canTransfer: rawCaps.canTransfer ?? false,
          actions: rawCaps.actions ?? [],
        };
      } else {
        this.capabilities = null;
      }

      // Load project roles for the role picker. Built-in membership roles
      // (owner/admin/member) go through the members API; custom project
      // roles are listed separately and assigned as role bindings.
      if (rolesRes.ok) {
        const rolesData = (await rolesRes.json()) as { items?: ProjectRole[] };
        const scoped = (rolesData.items || []).filter((r) => r.scopeType === 'project');
        this.projectRoles = scoped.filter((r) => !isCustomProjectRole(r.name));
        this.customRoles = scoped.filter((r) => isCustomProjectRole(r.name));
      }
    } catch (err) {
      console.error('Failed to load project members:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load project members';
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  // getPrincipalIcon is imported from ./role-binding-utils.js

  /** Effective read-only: true when the parent says read-only OR the server
   *  advisory indicates the current user cannot manage any membership tier. */
  private get effectiveReadOnly(): boolean {
    if (this.readOnly) return true;
    if (!this.capabilities) return true; // fail-closed when capabilities absent
    return !(
      this.capabilities.canManageMembers ||
      this.capabilities.canManageAdmins ||
      this.capabilities.canManageOwners
    );
  }

  /** Returns true if the current user can manage the given member based on
   *  the member's role tier and the user's capabilities. */
  private canManageMember(member: ProjectMemberBinding): boolean {
    if (!this.capabilities) return false;
    // Only owners may manage custom-role bindings; the server's governance
    // matrix limits admins to project-member.
    if (isCustomProjectRole(member.roleName)) return this.capabilities.canManageOwners;
    const tier = getRoleTier(member.roleName);
    switch (tier) {
      case 'owner':
        return this.capabilities.canManageOwners;
      case 'admin':
        return this.capabilities.canManageAdmins;
      case 'member':
        return this.capabilities.canManageMembers;
    }
  }

  /** Returns true if a role name represents project ownership. */
  private isOwnerRole(roleName: string): boolean {
    return PROJECT_OWNER_ROLE_NAMES.includes(roleName);
  }

  private get directOwnerCount(): number {
    return this.members.filter(
      (m) => m.source === 'direct' && m.principalType === 'user' && this.isOwnerRole(m.roleName)
    ).length;
  }

  private isLastDirectOwner(member: ProjectMemberBinding): boolean {
    if (member.source !== 'direct' || member.principalType !== 'user') return false;
    if (!this.isOwnerRole(member.roleName)) return false;
    return this.directOwnerCount <= 1;
  }

  /** Members grouped one row per principal and source, so a custom role
   *  shows beside the membership role instead of as a separate row. */
  private get memberRows(): MemberRow[] {
    return groupMemberRows(this.members);
  }

  /** Edit is available when the user may change the membership role or,
   *  as an owner, the custom roles. */
  private canEditRow(row: MemberRow): boolean {
    if (!this.capabilities) return false;
    if (row.primary && this.canManageMember(row.primary)) return true;
    return this.capabilities.canManageOwners && row.principalType !== 'agent';
  }

  /** Removal deletes every direct binding in the row, so an admin may not
   *  remove a member who also holds custom roles (only owners can remove
   *  those, and leaving them behind would keep the extra permissions). */
  private canRemoveRow(row: MemberRow): boolean {
    if (!this.capabilities) return false;
    if (row.primary && !this.canManageMember(row.primary)) return false;
    return row.custom.length === 0 || this.capabilities.canManageOwners;
  }

  private get addFilteredRoles(): ProjectRole[] {
    let roles = this.projectRoles;
    if (this.addPrincipalType === 'group') {
      roles = roles.filter((r) => !PROJECT_DIRECT_USER_ONLY_ROLES.includes(r.name));
    }
    // Filter by capabilities: only show roles the user can assign.
    if (this.capabilities) {
      const caps = this.capabilities;
      roles = roles.filter((r) => {
        const tier = getRoleTier(r.name);
        switch (tier) {
          case 'owner':
            return caps.canManageOwners;
          case 'admin':
            return caps.canManageAdmins;
          case 'member':
            return caps.canManageMembers;
        }
      });
    }
    return roles;
  }

  /** Custom roles the current user may offer in the add dialog: owners only,
   *  and never for agents, since a custom binding on an agent is a
   *  delegation grant, which the server refuses on this path. */
  private get addCustomRoles(): ProjectRole[] {
    if (!this.capabilities?.canManageOwners) return [];
    if (this.addPrincipalType === 'agent') return [];
    return this.customRoles;
  }

  private get addSelectedIsCustom(): boolean {
    return this.customRoles.some((r) => r.id === this.addRoleId);
  }

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  private openAddDialog(): void {
    this.addPrincipalType = 'user';
    this.addPrincipalId = '';
    this.addRoleId = this.projectRoles.length > 0 ? this.projectRoles[0].id : '';
    this.addError = null;
    this.addDialogOpen = true;
  }

  private async handleAddMember(): Promise<void> {
    if (!this.addPrincipalId.trim()) {
      this.addError = 'Please select a principal';
      return;
    }
    if (!this.addRoleId) {
      this.addError = 'Please select a role';
      return;
    }

    this.addLoading = true;
    this.addError = null;

    const isCustom = this.addSelectedIsCustom;

    try {
      // PM1: Built-in roles use the project-scoped members endpoint; custom
      // roles are plain project-scoped role bindings.
      // suppressAccessDeniedToast: the dialog renders errors inline (RC-C fix).
      const url = isCustom
        ? '/api/v1/admin/role-bindings'
        : `/api/v1/projects/${encodeURIComponent(this.projectId)}/members`;
      const body: Record<string, string> = {
        roleDefinitionId: this.addRoleId,
        principalType: this.addPrincipalType,
        principalId: this.addPrincipalId.trim(),
      };
      if (isCustom) {
        body.scopeType = 'project';
        body.scopeId = this.projectId;
      }
      const res = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        suppressAccessDeniedToast: true,
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        throw new Error(isCustom ? describeCustomRoleError(msg) : msg);
      }

      this.addDialogOpen = false;
      this.actionFeedback = {
        message: isCustom ? 'Role assigned' : 'Member added',
        variant: 'success',
      };
      void this.loadData();
    } catch (err) {
      console.error('Failed to add member:', err);
      this.addError = err instanceof Error ? err.message : 'Failed to add member';
    } finally {
      this.addLoading = false;
    }
  }

  private openChangeRoleDialog(row: MemberRow): void {
    this.changeRow = row;
    this.changeRoleId = row.primary?.roleDefinitionId ?? '';
    this.changeCustomIds = row.custom.map((b) => b.roleDefinitionId);
    this.changeError = null;
    this.changeDialogOpen = true;
  }

  private closeChangeDialog(): void {
    this.changeDialogOpen = false;
    this.changeRow = null;
    this.changeError = null;
  }

  /** Look up a role name from its definition ID. */
  private getRoleNameById(roleId: string): string {
    const role = this.projectRoles.find((r) => r.id === roleId);
    return role?.name ?? '';
  }

  /** Custom roles the edit dialog may offer for this row. */
  private editCustomRoles(row: MemberRow): ProjectRole[] {
    if (!this.capabilities?.canManageOwners || row.principalType === 'agent') return [];
    return this.customRoles;
  }

  private get changeHasChanges(): boolean {
    const row = this.changeRow;
    if (!row) return false;
    const plan = planMemberEdit(row, this.changeRoleId, this.changeCustomIds);
    return plan.membershipRoleId !== null || plan.add.length > 0 || plan.remove.length > 0;
  }

  private async handleChangeRole(): Promise<void> {
    const row = this.changeRow;
    if (!row) return;
    const plan = planMemberEdit(row, this.changeRoleId, this.changeCustomIds);

    // R1: Prevent demoting the last direct owner to a non-owner role.
    if (
      plan.membershipRoleId !== null &&
      row.primary &&
      this.isLastDirectOwner(row.primary) &&
      !this.isOwnerRole(this.getRoleNameById(plan.membershipRoleId))
    ) {
      this.changeError =
        'Cannot change the last direct project owner to a non-owner role. Transfer ownership first.';
      return;
    }

    this.changeLoading = true;
    this.changeError = null;
    const base = `/api/v1/projects/${encodeURIComponent(this.projectId)}/members`;
    const failures: string[] = [];

    // suppressAccessDeniedToast: the dialog renders errors inline (RC-C fix).
    if (plan.membershipRoleId !== null && row.primary) {
      const res = await apiFetch(`${base}/${row.primary.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ roleDefinitionId: plan.membershipRoleId }),
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) failures.push(await extractApiError(res, `HTTP ${res.status}`));
    }
    for (const roleId of plan.add) {
      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          roleDefinitionId: roleId,
          principalType: row.principalType,
          principalId: row.principalId,
          scopeType: 'project',
          scopeId: this.projectId,
        }),
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) {
        failures.push(describeCustomRoleError(await extractApiError(res, `HTTP ${res.status}`)));
      }
    }
    for (const binding of plan.remove) {
      const res = await apiFetch(`${base}/${binding.id}`, {
        method: 'DELETE',
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) failures.push(await extractApiError(res, `HTTP ${res.status}`));
    }

    this.changeLoading = false;
    void this.loadData();
    if (failures.length > 0) {
      // Keep the dialog open so the user sees what did not apply.
      this.changeError = failures.join(' ');
      return;
    }
    this.closeChangeDialog();
    this.actionFeedback = { message: 'Member updated', variant: 'success' };
  }

  private async handleRemoveMember(row: MemberRow): Promise<void> {
    if (row.primary && this.isLastDirectOwner(row.primary)) {
      this.actionFeedback = {
        message: 'Cannot remove the last direct project owner. Transfer ownership first.',
        variant: 'danger',
      };
      return;
    }

    if (
      !(await showConfirm(`Remove ${row.principalType} "${row.displayName}" from this project?`))
    ) {
      return;
    }

    this.removingMemberId = row.key;

    try {
      // PM1: Use project-scoped members endpoint. Custom-role bindings go
      // first: removing membership does not remove them, and they would
      // otherwise keep their permissions after the member is gone.
      // suppressAccessDeniedToast: inline alert handles errors (RC-C fix).
      const bindings = [...row.custom, ...(row.primary ? [row.primary] : [])];
      for (const binding of bindings) {
        const res = await apiFetch(
          `/api/v1/projects/${encodeURIComponent(this.projectId)}/members/${binding.id}`,
          { method: 'DELETE', suppressAccessDeniedToast: true }
        );
        if (!res.ok) {
          throw new Error(await extractApiError(res, `HTTP ${res.status}`));
        }
      }

      this.actionFeedback = { message: 'Member removed', variant: 'success' };
    } catch (err) {
      console.error('Failed to remove member:', err);
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to remove member',
        variant: 'danger',
      };
    } finally {
      this.removingMemberId = null;
      void this.loadData();
    }
  }

  // ---------------------------------------------------------------------------
  // Transfer ownership
  // ---------------------------------------------------------------------------

  private openTransferDialog(): void {
    this.transferNewOwnerId = '';
    this.transferError = null;
    this.transferDialogOpen = true;
  }

  private async handleTransferOwnership(): Promise<void> {
    if (!this.transferNewOwnerId.trim()) {
      this.transferError = 'Please enter a user ID or email';
      return;
    }

    this.transferLoading = true;
    this.transferError = null;

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/transfer-ownership`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            newOwnerId: this.transferNewOwnerId.trim(),
          }),
          suppressAccessDeniedToast: true,
        }
      );

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.transferDialogOpen = false;
      this.actionFeedback = {
        message: 'Ownership transferred successfully',
        variant: 'success',
      };
      void this.loadData();
    } catch (err) {
      console.error('Failed to transfer ownership:', err);
      this.transferError = err instanceof Error ? err.message : 'Failed to transfer ownership';
    } finally {
      this.transferLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderStandalone();
  }

  private renderStandalone() {
    return html`
      ${this.renderFeedback()}
      <div class="section-header">
        <div class="section-header-info">
          <h2>
            ${this.sectionTitle}
            <span class="member-count">(${this.memberRows.length})</span>
          </h2>
          ${this.sectionDescription ? html`<p>${this.sectionDescription}</p>` : nothing}
        </div>
        <div class="section-header-actions">
          ${this.capabilities?.canTransfer
            ? html`
                <sl-button variant="warning" size="small" outline @click=${this.openTransferDialog}>
                  <sl-icon slot="prefix" name="arrow-left-right"></sl-icon>
                  Transfer Ownership
                </sl-button>
              `
            : nothing}
          ${!this.effectiveReadOnly
            ? html`
                <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
      </div>
      ${this.renderBody()} ${this.renderAddDialog()} ${this.renderChangeRoleDialog()}
      ${this.renderTransferDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        ${this.renderFeedback()}
        <div class="section-header">
          <div class="section-header-info">
            <h2>
              ${this.sectionTitle}
              <span class="member-count">(${this.memberRows.length})</span>
            </h2>
            ${this.sectionDescription ? html`<p>${this.sectionDescription}</p>` : nothing}
          </div>
          <div class="section-header-actions">
            ${this.capabilities?.canTransfer
              ? html`
                  <sl-button
                    variant="warning"
                    size="small"
                    outline
                    @click=${this.openTransferDialog}
                  >
                    <sl-icon slot="prefix" name="arrow-left-right"></sl-icon>
                    Transfer Ownership
                  </sl-button>
                `
              : nothing}
            ${!this.effectiveReadOnly
              ? html`
                  <sl-button size="small" variant="default" @click=${this.openAddDialog}>
                    <sl-icon slot="prefix" name="person-plus"></sl-icon>
                    Add Member
                  </sl-button>
                `
              : nothing}
          </div>
        </div>
        ${this.renderBody()} ${this.renderAddDialog()} ${this.renderChangeRoleDialog()}
        ${this.renderTransferDialog()}
      </div>
    `;
  }

  private renderFeedback() {
    if (!this.actionFeedback) return nothing;
    return html`
      <sl-alert
        class="feedback-alert"
        variant=${this.actionFeedback.variant}
        open
        closable
        duration="5000"
        @sl-after-hide=${() => {
          this.actionFeedback = null;
        }}
      >
        <sl-icon
          slot="icon"
          name=${this.actionFeedback.variant === 'success'
            ? 'check-circle'
            : 'exclamation-triangle'}
        ></sl-icon>
        ${this.actionFeedback.message}
      </sl-alert>
    `;
  }

  private renderBody() {
    if (this.loading) {
      return html` <div class="loading-state"><sl-spinner></sl-spinner> Loading members...</div> `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadData()}> Retry </sl-button>
        </div>
      `;
    }

    if (this.members.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="people"></sl-icon>
          <h3>No Members</h3>
          <p>Add members to grant access to this project.</p>
          ${!this.effectiveReadOnly
            ? html`
                <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
      `;
    }

    return this.renderMembersTable();
  }

  private renderMembersTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Member</th>
              <th>Role</th>
              <th class="hide-mobile">Source</th>
              ${!this.effectiveReadOnly ? html`<th class="actions-cell">Actions</th>` : nothing}
            </tr>
          </thead>
          <tbody>
            ${this.memberRows.map((row) => this.renderMemberRow(row))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderMemberRow(row: MemberRow) {
    const isRemoving = this.removingMemberId === row.key;
    const isGroupDerived = row.source !== 'direct';
    const lastOwner = row.primary ? this.isLastDirectOwner(row.primary) : false;
    const canEdit = this.canEditRow(row);
    const canRemove = this.canRemoveRow(row);

    return html`
      <tr>
        <td>
          <div class="member-identity">
            <div class="member-icon ${row.principalType}">
              <sl-icon name="${getPrincipalIcon(row.principalType)}"></sl-icon>
            </div>
            <div class="member-info">
              <span class="member-name">${row.displayName}</span>
              <span class="member-detail">${row.principalType}</span>
            </div>
          </div>
        </td>
        <td>
          <div class="role-badges">
            ${row.primary ? html`<span class="role-badge">${row.primary.roleName}</span>` : nothing}
            ${row.custom.map(
              (b) => html`<span class="role-badge custom" title="Custom role">${b.roleName}</span>`
            )}
          </div>
        </td>
        <td class="hide-mobile">
          <span class="provenance-badge ${isGroupDerived ? 'group-derived' : 'direct'}">
            ${isGroupDerived
              ? html`<sl-icon name="diagram-3"></sl-icon> Via group:
                  ${row.sourceGroupName || row.source}`
              : html`<sl-icon name="person-check"></sl-icon> Direct`}
          </span>
        </td>
        ${!this.effectiveReadOnly
          ? html`
              <td class="actions-cell">
                ${isGroupDerived
                  ? html`<span class="meta-text">Inherited</span>`
                  : html`
                      ${canEdit
                        ? html`
                            <sl-icon-button
                              name="pencil"
                              label="Edit member roles"
                              ?disabled=${isRemoving}
                              @click=${() => this.openChangeRoleDialog(row)}
                            ></sl-icon-button>
                          `
                        : ''}
                      ${canRemove
                        ? html`
                            <sl-icon-button
                              name="trash"
                              label="Remove member"
                              ?disabled=${isRemoving || lastOwner}
                              @click=${() => this.handleRemoveMember(row)}
                            ></sl-icon-button>
                          `
                        : ''}
                      ${lastOwner
                        ? html`<sl-tooltip
                            content="Last direct owner — cannot change membership role or remove"
                          >
                            <sl-icon
                              name="shield-lock"
                              style="color: var(--sl-color-warning-500)"
                            ></sl-icon>
                          </sl-tooltip>`
                        : ''}
                    `}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderAddDialog() {
    if (!this.addDialogOpen) return nothing;

    return html`
      <sl-dialog
        label="Add Project Member"
        open
        @sl-request-close=${() => {
          if (!this.addLoading) this.addDialogOpen = false;
        }}
      >
        <div class="form-group">
          <sl-select
            label="Member Type"
            hoist
            .value=${this.addPrincipalType}
            @sl-change=${(e: Event) => {
              this.addPrincipalType = (e.target as HTMLSelectElement).value;
              this.addPrincipalId = '';
              if (this.addSelectedIsCustom && this.addCustomRoles.length === 0) {
                this.addRoleId = this.addFilteredRoles[0]?.id ?? '';
              }
            }}
          >
            <sl-option value="user">
              <sl-icon slot="prefix" name="person"></sl-icon>
              User
            </sl-option>
            <sl-option value="agent">
              <sl-icon slot="prefix" name="cpu"></sl-icon>
              Agent
            </sl-option>
            <sl-option value="group">
              <sl-icon slot="prefix" name="diagram-3"></sl-icon>
              Group
            </sl-option>
          </sl-select>
        </div>

        <div class="form-group">
          <scion-principal-picker
            .principalType=${this.addPrincipalType as 'user' | 'agent' | 'group'}
            @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) => {
              this.addPrincipalId = e.detail.principalId;
            }}
          ></scion-principal-picker>
        </div>

        ${this.projectRoles.length > 0
          ? html`
              <div class="form-group">
                <sl-select
                  label="Project Role"
                  hoist
                  .value=${this.addRoleId}
                  @sl-change=${(e: Event) => {
                    this.addRoleId = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${this.addCustomRoles.length > 0
                    ? html`<small>Membership roles</small>`
                    : nothing}
                  ${this.addFilteredRoles.map(
                    (role) => html` <sl-option value=${role.id}>${role.name}</sl-option> `
                  )}
                  ${this.addCustomRoles.length > 0
                    ? html`
                        <sl-divider></sl-divider>
                        <small>Custom roles</small>
                        ${this.addCustomRoles.map(
                          (role) => html` <sl-option value=${role.id}>${role.name}</sl-option> `
                        )}
                      `
                    : nothing}
                </sl-select>
              </div>
              ${this.addSelectedIsCustom
                ? html`
                    <div class="validation-warning">
                      <sl-icon name="info-circle"></sl-icon>
                      A custom role adds permissions on top of a membership role; on its own it does
                      not make someone a member. You can only grant permissions you hold yourself.
                    </div>
                  `
                : ''}
              ${this.addPrincipalType === 'group'
                ? html`
                    <div class="validation-warning">
                      <sl-icon name="info-circle"></sl-icon>
                      Group members will inherit this project role. Owner role is not available for
                      groups.
                    </div>
                  `
                : ''}
            `
          : ''}
        ${this.addError ? html`<div class="dialog-error">${this.addError}</div>` : nothing}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.addLoading}
          @click=${() => {
            this.addDialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.addLoading}
          ?disabled=${!this.addPrincipalId.trim()}
          @click=${() => this.handleAddMember()}
          >Add Member</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderChangeRoleDialog() {
    const row = this.changeRow;
    if (!this.changeDialogOpen || !row) return nothing;

    const lastOwner = row.primary ? this.isLastDirectOwner(row.primary) : false;
    const canChangeMembership = !!row.primary && this.canManageMember(row.primary) && !lastOwner;
    const membershipRoles = this.projectRoles.filter(
      (r) => row.principalType !== 'group' || !PROJECT_DIRECT_USER_ONLY_ROLES.includes(r.name)
    );
    const customRoles = this.editCustomRoles(row);

    return html`
      <sl-dialog
        label="Edit Member"
        open
        @sl-request-close=${() => {
          if (!this.changeLoading) this.closeChangeDialog();
        }}
      >
        <div class="dialog-member">
          <div class="member-icon ${row.principalType}">
            <sl-icon name="${getPrincipalIcon(row.principalType)}"></sl-icon>
          </div>
          <div class="member-info">
            <span class="member-name">${row.displayName}</span>
            <span class="member-detail">${row.principalType}</span>
          </div>
        </div>

        ${row.primary
          ? html`
              <div class="form-group">
                <sl-select
                  label="Project Role"
                  hoist
                  .value=${this.changeRoleId}
                  ?disabled=${!canChangeMembership || this.changeLoading}
                  @sl-change=${(e: Event) => {
                    this.changeRoleId = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${membershipRoles.map(
                    (role) => html` <sl-option value=${role.id}>${role.name}</sl-option> `
                  )}
                </sl-select>
                ${lastOwner
                  ? html`<p class="form-help">
                      This is the last direct owner. Transfer ownership to change this role.
                    </p>`
                  : nothing}
              </div>
            `
          : html`<p class="form-help">
              This ${row.principalType} holds only custom roles here, so it is not a project member.
              Add it as a member to give it a project role.
            </p>`}
        ${customRoles.length > 0
          ? html`
              <div class="form-group">
                <span class="form-label">Custom Roles</span>
                <div class="custom-role-list">
                  ${customRoles.map(
                    (role) => html`
                      <sl-checkbox
                        ?checked=${this.changeCustomIds.includes(role.id)}
                        ?disabled=${this.changeLoading}
                        @sl-change=${(e: Event) => {
                          const on = (e.target as HTMLInputElement).checked;
                          this.changeCustomIds = on
                            ? [...this.changeCustomIds, role.id]
                            : this.changeCustomIds.filter((id) => id !== role.id);
                        }}
                        >${role.name}</sl-checkbox
                      >
                    `
                  )}
                </div>
                <p class="form-help">
                  Custom roles add permissions on top of the project role. You can only grant
                  permissions you hold yourself.
                </p>
              </div>
            `
          : nothing}
        ${this.changeError ? html`<div class="dialog-error">${this.changeError}</div>` : nothing}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.changeLoading}
          @click=${() => this.closeChangeDialog()}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.changeLoading}
          ?disabled=${!this.changeHasChanges}
          @click=${() => this.handleChangeRole()}
          >Save</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderTransferDialog() {
    if (!this.transferDialogOpen) return nothing;

    return html`
      <sl-dialog
        label="Transfer Project Ownership"
        open
        @sl-request-close=${() => {
          if (!this.transferLoading) this.transferDialogOpen = false;
        }}
      >
        <p>
          Transfer ownership of this project to another user. The current owner will retain
          membership but lose owner privileges.
        </p>
        <div class="form-group">
          <sl-input
            label="New Owner (User ID or Email)"
            placeholder="Enter user ID or email address"
            .value=${this.transferNewOwnerId}
            @sl-input=${(e: Event) => {
              this.transferNewOwnerId = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>

        ${this.transferError
          ? html`<div class="dialog-error">${this.transferError}</div>`
          : nothing}

        <div class="validation-warning">
          <sl-icon name="exclamation-triangle"></sl-icon>
          This action cannot be undone. The new owner will have full control of this project.
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.transferLoading}
          @click=${() => {
            this.transferDialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.transferLoading}
          ?disabled=${!this.transferNewOwnerId.trim()}
          @click=${() => this.handleTransferOwnership()}
          >Transfer Ownership</sl-button
        >
      </sl-dialog>
    `;
  }
}

/** Group bindings into one row per principal and source. The membership
 *  binding (a built-in role) becomes the row's primary; custom-role bindings
 *  are listed beside it. Order follows the first binding seen. */
export function groupMemberRows(members: ProjectMemberBinding[]): MemberRow[] {
  const rows = new Map<string, MemberRow>();
  for (const m of members) {
    const key = `${m.source}|${m.principalType}|${m.principalId}`;
    let row = rows.get(key);
    if (!row) {
      row = {
        key,
        primary: null,
        custom: [],
        principalType: m.principalType,
        principalId: m.principalId,
        displayName: m.principalDisplayName || m.principalId,
        source: m.source,
        sourceGroupName: m.sourceGroupName,
      };
      rows.set(key, row);
    }
    if (isCustomProjectRole(m.roleName)) {
      row.custom.push(m);
    } else if (!row.primary) {
      row.primary = m;
    } else {
      // Two membership bindings for one principal should not happen; keep
      // the extra visible rather than hiding it.
      row.custom.push(m);
    }
    if (m.principalDisplayName) row.displayName = m.principalDisplayName;
  }
  return [...rows.values()];
}

export interface MemberEditPlan {
  /** New membership role ID, or null when unchanged. */
  membershipRoleId: string | null;
  /** Custom role definition IDs to bind. */
  add: string[];
  /** Custom-role bindings to delete. */
  remove: ProjectMemberBinding[];
}

/** Work out the requests needed to move a row to the chosen roles. */
export function planMemberEdit(
  row: MemberRow,
  membershipRoleId: string,
  customRoleIds: string[]
): MemberEditPlan {
  const held = new Set(row.custom.map((b) => b.roleDefinitionId));
  const wanted = new Set(customRoleIds);
  return {
    membershipRoleId:
      row.primary && membershipRoleId && membershipRoleId !== row.primary.roleDefinitionId
        ? membershipRoleId
        : null,
    add: customRoleIds.filter((id) => !held.has(id)),
    remove: row.custom.filter((b) => !wanted.has(b.roleDefinitionId)),
  };
}

/** True for project roles other than the built-in membership roles. */
export function isCustomProjectRole(roleName: string): boolean {
  return !BUILT_IN_PROJECT_MEMBERSHIP_ROLES.includes(roleName);
}

/** Turns the server's delegation-ceiling refusal into guidance; other errors
 *  pass through unchanged. */
export function describeCustomRoleError(message: string): string {
  const m = /lacks permission for delegation: ([\w.:-]+)/.exec(message);
  if (!m) return message;
  return `You can't assign this role: it includes the permission "${m[1]}", which you don't hold yourself. Ask a hub admin to assign it.`;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-members-editor': ScionProjectMembersEditor;
  }
}
