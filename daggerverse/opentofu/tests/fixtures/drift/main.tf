terraform {
  required_providers {
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

variable "content" {
  type        = string
  default     = "managed by tofu\n"
  description = "What the managed file holds."
}

# The one hermetic fixture with an object outside state that can drift. On
# refresh, local_file forgets a file whose checksum no longer matches its id,
# so changing managed.txt in the source directory is a change made outside
# tofu. Apply's result does not carry the file back, so a test re-creates it
# alongside the configuration to stand for an object nobody touched.
resource "local_file" "managed" {
  filename = "${path.module}/managed.txt"
  content  = var.content
}
