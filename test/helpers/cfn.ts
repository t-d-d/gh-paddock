import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import yaml from 'js-yaml';
import { makeCfnSchema, type Substitutions, type Template } from './cfn-schema.ts';

export { resourcesOfType, type Resource, type Statement, type Template } from './cfn-schema.ts';

/** The repo's root, for paths the tests read. */
export const REPO_ROOT = resolve(import.meta.dirname, '../..');

/** Parse a CloudFormation template from the repo, intrinsic tags and all. */
export function loadTemplate(path: string, substitutions: Substitutions = {}): Template {
  return yaml.load(readFileSync(resolve(REPO_ROOT, path), 'utf-8'), {
    schema: makeCfnSchema(substitutions),
  }) as Template;
}

/** The one resource of `type` in `tpl`, failing the test if there is not exactly one. */
export function onlyResource(tpl: Template, type: string) {
  const found = Object.entries(tpl.Resources ?? {}).filter(([, r]) => r.Type === type);
  if (found.length !== 1) throw new Error(`expected one ${type}, found ${found.length}`);
  return found[0]![1];
}
