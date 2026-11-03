import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import yaml from 'js-yaml';
import { REPO_ROOT } from './cfn.ts';

export type WorkflowStep = {
  id?: string;
  name?: string;
  if?: string;
  uses?: string;
  run?: string;
  env?: Record<string, unknown>;
  with?: Record<string, unknown>;
};

/**
 * A workflow or composite action for a test: the parsed YAML, and a
 * comment-free view of the file, so assertions read the commands and not the
 * header prose, which can name the very strings under test. Line
 * continuations are joined so a flag split across lines matches as one.
 */
export function loadWorkflow(path: string): { raw: string; codeOnly: string; workflow: Record<string, any> } {
  const raw = readFileSync(resolve(REPO_ROOT, path), 'utf-8');
  const codeOnly = raw
    .split('\n')
    .filter((line) => !/^\s*#/.test(line))
    .join('\n')
    .replace(/\\\n\s*/g, ' ');
  return { raw, codeOnly, workflow: yaml.load(raw) as Record<string, any> };
}

/** The steps of one job, in order. */
export const stepsOf = (workflow: Record<string, any>, job: string): WorkflowStep[] => workflow.jobs?.[job]?.steps ?? [];

/** The step with `id`, failing the test if there is none. */
export function stepById(steps: WorkflowStep[], id: string): WorkflowStep {
  const step = steps.find((s) => s.id === id);
  if (!step) throw new Error(`no step with id ${id}`);
  return step;
}
