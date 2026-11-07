// What the CLI's discovery run (ANVIL_BUILD_MODE) needs to know about an Org.
import * as fs from 'fs';

/** True during the CLI's discovery run, which never creates resources. */
export function isDiscovery(): boolean {
  return process.env.ANVIL_BUILD_MODE === 'true';
}

/**
 * Tells the CLI this program is an Org during its discovery run
 * (ANVIL_BUILD_MODE). Must match cmd/anvil/cmd/org.go.
 */
export function writeOrgSettings(): void {
  const path = process.env.ANVIL_APP_SETTINGS_PATH;
  if (isDiscovery() && path) {
    fs.writeFileSync(path, JSON.stringify({ kind: 'org' }, null, 2));
  }
}
