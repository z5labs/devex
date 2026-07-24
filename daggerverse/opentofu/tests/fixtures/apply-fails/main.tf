terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

# The configuration is valid and the plan succeeds; the provider only fails at
# apply time, when it tries to create a file under a path that cannot exist.
# That is the shape of a real partial-apply failure.
resource "local_file" "unwritable" {
  filename = "/proc/devex/opentofu/unwritable.txt"
  content  = "this can never be written"
}
