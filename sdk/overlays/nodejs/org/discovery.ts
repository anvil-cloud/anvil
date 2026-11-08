// What the CLI's discovery run (ANVIL_BUILD_MODE) needs to know about an Org.
import * as fs from 'fs';
import { PlannedOu } from './ous/plan';
import { PlannedAccount } from './accounts/plan';

/** True during the CLI's discovery run, which never creates resources. */
export function isDiscovery(): boolean {
  return process.env.ANVIL_BUILD_MODE === 'true';
}

/**
 * Tells the CLI this program is an Org during its discovery run
 * (ANVIL_BUILD_MODE), with the planned OUs and accounts so the CLI can check
 * renames against state before deploying. Must match cmd/anvil/cmd/org_project.go.
 */
export function writeOrgSettings(ous: PlannedOu[], accounts: PlannedAccount[]): void {
  const path = process.env.ANVIL_APP_SETTINGS_PATH;
  if (isDiscovery() && path) {
    const settings = {
      kind: 'org',
      ous: ous.map((o) => ({ path: o.path, ...(o.previousPath ? { previousPath: o.previousPath } : {}) })),
      accounts: accounts.map((a) => ({
        name: a.name,
        ou: a.ou,
        kind: a.kind,
        ...(a.accountId ? { accountId: a.accountId } : {}),
        ...(a.renamedFrom ? { renamedFrom: a.renamedFrom } : {}),
      })),
    };
    fs.writeFileSync(path, JSON.stringify(settings, null, 2));
  }
}
