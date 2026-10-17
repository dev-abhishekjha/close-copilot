import type { Role } from '../users';

// Ids of the seeded rows (e2e/fixture/seed.go: sid(prefix, n)).
export const id = {
  runDone: 'a1e20000-0000-0000-0000-000000000001',
  runFailed: 'a1e20000-0000-0000-0000-000000000002',
  runQueued: 'a1e20000-0000-0000-0000-000000000003',
  runOlder: 'a1e20000-0000-0000-0000-000000000004',
  findClean: 'b1e20000-0000-0000-0000-000000000001',
  findReview: 'b1e20000-0000-0000-0000-000000000002',
  findEntry: 'b1e20000-0000-0000-0000-000000000003',
  findPending: 'b1e20000-0000-0000-0000-000000000004',
  propPending: 'd1e20000-0000-0000-0000-000000000001',
  propPosted: 'd1e20000-0000-0000-0000-000000000003',
  propRejected: 'd1e20000-0000-0000-0000-000000000004',
};

const all: Role[] = ['viewer', 'maker', 'checker'];

export interface PageCase {
  name: string; // also the screenshot's file name
  path: string;
  roles: Role[];
}

// Every page the suite covers, with the roles that may open it. A new page
// goes here and nowhere else: a11y, keyboard, responsive, CSP and screenshot
// specs all read this list.
export const pages: PageCase[] = [
  { name: 'dashboard', path: '/', roles: all },
  { name: 'runs', path: '/runs', roles: all },
  { name: 'run-done', path: `/runs/${id.runDone}`, roles: all },
  { name: 'run-failed', path: `/runs/${id.runFailed}`, roles: all },
  { name: 'run-queued', path: `/runs/${id.runQueued}`, roles: all },
  { name: 'run-findings', path: `/runs/${id.runDone}/findings`, roles: all },
  { name: 'run-metrics', path: `/runs/${id.runDone}/metrics`, roles: all },
  { name: 'findings', path: '/findings', roles: all },
  { name: 'finding-verified', path: `/findings/${id.findClean}`, roles: all },
  { name: 'finding-review', path: `/findings/${id.findReview}`, roles: all },
  { name: 'finding-entry', path: `/findings/${id.findEntry}`, roles: all },
  { name: 'proposal-pending', path: `/proposals/${id.propPending}`, roles: all },
  { name: 'proposal-posted', path: `/proposals/${id.propPosted}`, roles: all },
  { name: 'proposal-rejected', path: `/proposals/${id.propRejected}`, roles: all },
  { name: 'ask', path: '/ask', roles: all },
  { name: 'documents', path: '/documents', roles: all },
  { name: 'document', path: '/documents/POL-001', roles: all },
  { name: 'system', path: '/system', roles: all },
  { name: 'approvals', path: '/approvals', roles: ['checker'] },
  { name: 'audit', path: '/audit', roles: ['checker'] },
];

export const forRole = (role: Role) => pages.filter((p) => p.roles.includes(role));
