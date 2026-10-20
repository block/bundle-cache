import * as core from "@actions/core";
import * as exec from "@actions/exec";
import {
  backendArgs,
  commonArgs,
  gradleHomeArgs,
  includedBuildArgs,
  gitDirArgs,
  execOptions,
  resolveBranch,
  defaultBranch,
  isDefaultBranchBuild,
} from "./helpers.js";

async function run() {
  try {
    // Always persist save inputs so the post step runs regardless of restore.
    core.saveState("cache-key", core.getInput("cache-key"));
    core.saveState("save", core.getInput("save"));

    if (isDefaultBranchBuild()) {
      core.info(
        "Cache restore skipped — default-branch build publishes a fresh base bundle",
      );
      return;
    }

    const args = [
      "restore",
      ...commonArgs(),
      ...backendArgs(),
      ...gradleHomeArgs(),
      ...includedBuildArgs(),
      ...gitDirArgs(),
      "--ref",
      core.getInput("ref") || defaultBranch(),
    ];

    const branch = resolveBranch();
    if (branch) {
      args.push("--branch", branch);
      core.info(`Delta cache enabled for branch: ${branch}`);
    }

    const exitCode = await exec.exec("gradle-cache", args, execOptions({
      ignoreReturnCode: true,
    }));
    if (exitCode !== 0) {
      core.warning("Cache restore failed; proceeding without cache");
    }
  } catch (error) {
    core.warning(`Cache restore failed: ${error.message}`);
  }
}

run();
