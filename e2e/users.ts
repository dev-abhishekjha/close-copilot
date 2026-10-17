// The fixture accounts (e2e/fixture/fixture.go). Synthetic: they only work
// against the in-memory fixture on localhost.
export type Role = 'viewer' | 'maker' | 'checker';

export const users: Record<Role, { name: string; password: string }> = {
  viewer: { name: 'viewer', password: 'e2e-viewer-password' },
  maker: { name: 'asha', password: 'e2e-maker-password' },
  checker: { name: 'ravi', password: 'e2e-checker-password' },
};

export const authFile = (role: Role) => `.auth/${role}.json`;
