const { execSync } = require("child_process");
/* execSync("gh issue list") would 403 in a cloud session */
// gh pr list is GraphQL
execSync("gh pr list --json number");
console.log("gh pr merge is not ours to run");
