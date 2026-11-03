import { realpathSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

/**
 * Whether a module is the process entry point: the guard every `scripts/`
 * CLI puts between its exports and its `main()`. Copied from gh-governor.
 *
 * It is shared rather than copied per script because its failure is silent.
 * A miss throws nothing and prints nothing: `main()` never runs and the
 * process exits 0, which the deploy reads as a green probe.
 *
 * Neither comparison is a string compare. `pathToFileURL` percent-encodes
 * where the argv path does not, so a checkout under a path with a space
 * fails a naive compare. `realpathSync` is the second try because node
 * resolves the main module's symlinks before building `import.meta.url` and
 * leaves `process.argv[1]` as it was given. There is no basename fallback:
 * it would say yes to a same-named script anywhere on disk.
 *
 * @param {string} moduleUrl `import.meta.url` of the module asking.
 * @param {string | undefined} argv1 `process.argv[1]`, absent when node was
 *   given no script at all (`node -e`, a REPL), which is never the entry point.
 * @returns {boolean}
 */
export const isCliEntry = (moduleUrl, argv1) => {
  if (argv1 === undefined) return false;
  try {
    if (moduleUrl === pathToFileURL(argv1).href) return true;
    // A path that does not exist throws, and is not this module.
    return moduleUrl === pathToFileURL(realpathSync(argv1)).href;
  } catch {
    return false;
  }
};
