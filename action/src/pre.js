import * as core from "@actions/core";
import { install } from "./helpers.js";

async function run() {
  try {
    await install();
  } catch (error) {
    core.setFailed(`Failed to install gradle-cache: ${error.message}`);
  }
}

run();
