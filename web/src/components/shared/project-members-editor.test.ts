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
 * Project Members Editor — custom project roles
 *
 * Owners can assign custom project roles from the members editor. Built-in
 * roles still go through the members API; custom roles are posted as
 * project-scoped role bindings, are offered to owners only and never for
 * agents, and cannot be changed in place (only removed).
 */

import { describe, it, expect, vi, beforeEach } from 'vitest';

import { apiFetch } from '../../client/api.js';
import type { MembershipCapabilities } from '../../shared/types.js';
import {
  ScionProjectMembersEditor,
  describeCustomRoleError,
  groupMemberRows,
  isCustomProjectRole,
  planMemberEdit,
  type MemberRow,
  type ProjectMemberBinding,
} from './project-members-editor.js';

vi.mock('../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../client/api.js')>()),
  apiFetch: vi.fn(),
}));

vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

const OWNER_CAPS: MembershipCapabilities = {
  canManageMembers: true,
  canManageAdmins: true,
  canManageOwners: true,
  canTransfer: true,
  actions: [],
};

const ADMIN_CAPS: MembershipCapabilities = {
  canManageMembers: true,
  canManageAdmins: false,
  canManageOwners: false,
  canTransfer: false,
  actions: [],
};

const BUILT_IN = [
  { id: 'r-owner', name: 'project-owner', scopeType: 'project' },
  { id: 'r-admin', name: 'project-admin', scopeType: 'project' },
  { id: 'r-member', name: 'project-member', scopeType: 'project' },
];
const CUSTOM = [{ id: 'r-msg', name: 'project-member-messaging', scopeType: 'project' }];

/** Private surface the tests drive directly. */
interface EditorInternals {
  capabilities: MembershipCapabilities | null;
  projectRoles: typeof BUILT_IN;
  customRoles: typeof CUSTOM;
  addPrincipalType: string;
  addPrincipalId: string;
  addRoleId: string;
  addError: string | null;
  addCustomRoles: typeof CUSTOM;
  loadData(): Promise<void>;
  handleAddMember(): Promise<void>;
  canManageMember(m: { roleName: string }): boolean;
  canRemoveRow(row: MemberRow): boolean;
  canEditRow(row: MemberRow): boolean;
  openChangeRoleDialog(row: MemberRow): void;
  changeRoleId: string;
  changeCustomIds: string[];
  changeError: string | null;
  changeDialogOpen: boolean;
  handleChangeRole(): Promise<void>;
  handleRemoveMember(row: MemberRow): Promise<void>;
  members: ProjectMemberBinding[];
}

function makeEditor(caps: MembershipCapabilities | null): EditorInternals {
  const el = new ScionProjectMembersEditor();
  el.projectId = 'p-1';
  const i = el as unknown as EditorInternals;
  i.capabilities = caps;
  i.projectRoles = BUILT_IN;
  i.customRoles = CUSTOM;
  // Stop the post-success reload from issuing further requests.
  i.loadData = async () => {};
  return i;
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
});

describe('isCustomProjectRole', () => {
  it('treats only non-built-in names as custom', () => {
    expect(isCustomProjectRole('project-owner')).toBe(false);
    expect(isCustomProjectRole('project-admin')).toBe(false);
    expect(isCustomProjectRole('project-member')).toBe(false);
    expect(isCustomProjectRole('project-member-messaging')).toBe(true);
  });
});

describe('loadData role split', () => {
  it('separates built-in and custom project roles and drops system roles', async () => {
    const el = new ScionProjectMembersEditor();
    el.projectId = 'p-1';
    vi.mocked(apiFetch).mockImplementation(async (url: string) =>
      url.includes('/members')
        ? jsonResponse(200, { items: [], _capabilities: OWNER_CAPS })
        : jsonResponse(200, {
            items: [
              ...BUILT_IN,
              ...CUSTOM,
              { id: 'r-hub', name: 'hub-member', scopeType: 'system' },
            ],
          })
    );
    const i = el as unknown as EditorInternals;
    await i.loadData();
    expect(i.projectRoles.map((r) => r.name)).toEqual([
      'project-owner',
      'project-admin',
      'project-member',
    ]);
    expect(i.customRoles.map((r) => r.name)).toEqual(['project-member-messaging']);
  });
});

describe('custom roles offered in the add dialog', () => {
  it('offers custom roles to owners for users and groups', () => {
    const el = makeEditor(OWNER_CAPS);
    el.addPrincipalType = 'user';
    expect(el.addCustomRoles).toHaveLength(1);
    el.addPrincipalType = 'group';
    expect(el.addCustomRoles).toHaveLength(1);
  });

  it('never offers custom roles for agents', () => {
    const el = makeEditor(OWNER_CAPS);
    el.addPrincipalType = 'agent';
    expect(el.addCustomRoles).toHaveLength(0);
  });

  it('does not offer custom roles to project admins', () => {
    const el = makeEditor(ADMIN_CAPS);
    el.addPrincipalType = 'user';
    expect(el.addCustomRoles).toHaveLength(0);
  });
});

describe('handleAddMember routing', () => {
  it('posts built-in roles to the members API', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.addPrincipalId = 'u-1';
    el.addRoleId = 'r-member';
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse(201, {}));

    await el.handleAddMember();

    const [url, init] = vi.mocked(apiFetch).mock.calls[0];
    expect(url).toBe('/api/v1/projects/p-1/members');
    expect(JSON.parse(init!.body as string)).toEqual({
      roleDefinitionId: 'r-member',
      principalType: 'user',
      principalId: 'u-1',
    });
  });

  it('posts custom roles as project-scoped role bindings', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.addPrincipalId = 'u-1';
    el.addRoleId = 'r-msg';
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse(201, {}));

    await el.handleAddMember();

    const [url, init] = vi.mocked(apiFetch).mock.calls[0];
    expect(url).toBe('/api/v1/admin/role-bindings');
    expect(JSON.parse(init!.body as string)).toEqual({
      roleDefinitionId: 'r-msg',
      principalType: 'user',
      principalId: 'u-1',
      scopeType: 'project',
      scopeId: 'p-1',
    });
    expect(init!.suppressAccessDeniedToast).toBe(true);
    expect(el.addError).toBeNull();
  });

  it('shows a delegation-ceiling refusal inline as guidance', async () => {
    const el = makeEditor(OWNER_CAPS);
    el.addPrincipalId = 'u-1';
    el.addRoleId = 'r-msg';
    vi.mocked(apiFetch).mockResolvedValue(
      jsonResponse(403, {
        error: {
          code: 'forbidden',
          message: 'cannot create binding: actor lacks permission for delegation: agent.attach',
        },
      })
    );

    await el.handleAddMember();

    expect(el.addError).toContain('"agent.attach"');
    expect(el.addError).toContain("which you don't hold yourself");
  });
});

describe('describeCustomRoleError', () => {
  it('passes unrelated errors through unchanged', () => {
    expect(describeCustomRoleError('role binding already exists')).toBe(
      'role binding already exists'
    );
  });
});

describe('managing custom-role rows', () => {
  it('lets owners manage custom-role bindings', () => {
    expect(makeEditor(OWNER_CAPS).canManageMember({ roleName: 'project-member-messaging' })).toBe(
      true
    );
  });

  it('does not let admins manage custom-role bindings', () => {
    const el = makeEditor(ADMIN_CAPS);
    expect(el.canManageMember({ roleName: 'project-member-messaging' })).toBe(false);
    expect(el.canManageMember({ roleName: 'project-member' })).toBe(true);
  });
});

function binding(
  id: string,
  principalId: string,
  roleDefinitionId: string,
  roleName: string,
  source = 'direct'
): ProjectMemberBinding {
  return {
    id,
    roleDefinitionId,
    roleName,
    principalType: 'user',
    principalId,
    principalDisplayName: principalId.toUpperCase(),
    scopeType: 'project',
    scopeId: 'p-1',
    createdAt: '2026-09-25T00:00:00Z',
    source,
  };
}

describe('groupMemberRows', () => {
  it('puts a principal and its custom roles on one row', () => {
    const rows = groupMemberRows([
      binding('b1', 'u-a', 'r-member', 'project-member'),
      binding('b2', 'u-b', 'r-owner', 'project-owner'),
      binding('b3', 'u-a', 'r-msg', 'project-member-messaging'),
    ]);
    expect(rows).toHaveLength(2);
    expect(rows[0].primary?.id).toBe('b1');
    expect(rows[0].custom.map((b) => b.id)).toEqual(['b3']);
    expect(rows[1].primary?.id).toBe('b2');
    expect(rows[1].custom).toEqual([]);
  });

  it('keeps custom-role-only principals as rows without a membership role', () => {
    const rows = groupMemberRows([binding('b1', 'u-a', 'r-msg', 'project-member-messaging')]);
    expect(rows).toHaveLength(1);
    expect(rows[0].primary).toBeNull();
    expect(rows[0].custom).toHaveLength(1);
  });

  it('does not merge direct and group-derived bindings', () => {
    const rows = groupMemberRows([
      binding('b1', 'u-a', 'r-member', 'project-member'),
      binding('b2', 'u-a', 'r-admin', 'project-admin', 'g-team'),
    ]);
    expect(rows).toHaveLength(2);
  });
});

describe('planMemberEdit', () => {
  const row = groupMemberRows([
    binding('b1', 'u-a', 'r-member', 'project-member'),
    binding('b3', 'u-a', 'r-msg', 'project-member-messaging'),
  ])[0];

  it('is empty when nothing changed', () => {
    expect(planMemberEdit(row, 'r-member', ['r-msg'])).toEqual({
      membershipRoleId: null,
      add: [],
      remove: [],
    });
  });

  it('reports a membership change and custom adds/removes', () => {
    const plan = planMemberEdit(row, 'r-admin', ['r-port']);
    expect(plan.membershipRoleId).toBe('r-admin');
    expect(plan.add).toEqual(['r-port']);
    expect(plan.remove.map((b) => b.id)).toEqual(['b3']);
  });
});

describe('member row permissions', () => {
  const withCustom = groupMemberRows([
    binding('b1', 'u-a', 'r-member', 'project-member'),
    binding('b3', 'u-a', 'r-msg', 'project-member-messaging'),
  ])[0];
  const plain = groupMemberRows([binding('b1', 'u-a', 'r-member', 'project-member')])[0];

  it('lets an admin remove a plain member but not one with custom roles', () => {
    const el = makeEditor(ADMIN_CAPS);
    expect(el.canRemoveRow(plain)).toBe(true);
    expect(el.canRemoveRow(withCustom)).toBe(false);
  });

  it('lets an owner remove and edit a member with custom roles', () => {
    const el = makeEditor(OWNER_CAPS);
    expect(el.canRemoveRow(withCustom)).toBe(true);
    expect(el.canEditRow(withCustom)).toBe(true);
  });
});

describe('edit dialog save', () => {
  it('patches membership, binds added custom roles and deletes removed ones', async () => {
    const el = makeEditor(OWNER_CAPS);
    const row = groupMemberRows([
      binding('b1', 'u-a', 'r-member', 'project-member'),
      binding('b3', 'u-a', 'r-msg', 'project-member-messaging'),
    ])[0];
    el.customRoles = [...CUSTOM, { id: 'r-port', name: 'port-viewer', scopeType: 'project' }];
    el.openChangeRoleDialog(row);
    el.changeRoleId = 'r-admin';
    el.changeCustomIds = ['r-port'];
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse(200, {}));

    await el.handleChangeRole();

    const calls = vi.mocked(apiFetch).mock.calls.map(([url, init]) => [url, init?.method]);
    expect(calls).toEqual([
      ['/api/v1/projects/p-1/members/b1', 'PATCH'],
      ['/api/v1/admin/role-bindings', 'POST'],
      ['/api/v1/projects/p-1/members/b3', 'DELETE'],
    ]);
    expect(JSON.parse(vi.mocked(apiFetch).mock.calls[1][1]!.body as string)).toEqual({
      roleDefinitionId: 'r-port',
      principalType: 'user',
      principalId: 'u-a',
      scopeType: 'project',
      scopeId: 'p-1',
    });
    expect(el.changeDialogOpen).toBe(false);
  });

  it('keeps the dialog open with guidance when the ceiling refuses a role', async () => {
    const el = makeEditor(OWNER_CAPS);
    const row = groupMemberRows([binding('b1', 'u-a', 'r-member', 'project-member')])[0];
    el.openChangeRoleDialog(row);
    el.changeCustomIds = ['r-msg'];
    vi.mocked(apiFetch).mockResolvedValue(
      jsonResponse(403, {
        error: {
          message: 'cannot create binding: actor lacks permission for delegation: agent.attach',
        },
      })
    );

    await el.handleChangeRole();

    expect(el.changeDialogOpen).toBe(true);
    expect(el.changeError).toContain('"agent.attach"');
  });
});

describe('removing a member', () => {
  it('deletes custom-role bindings before the membership binding', async () => {
    const el = makeEditor(OWNER_CAPS);
    const row = groupMemberRows([
      binding('b1', 'u-a', 'r-member', 'project-member'),
      binding('b3', 'u-a', 'r-msg', 'project-member-messaging'),
    ])[0];
    vi.mocked(apiFetch).mockResolvedValue(new Response(null, { status: 204 }));

    await el.handleRemoveMember(row);

    expect(vi.mocked(apiFetch).mock.calls.map(([url]) => url)).toEqual([
      '/api/v1/projects/p-1/members/b3',
      '/api/v1/projects/p-1/members/b1',
    ]);
  });
});
