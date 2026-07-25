terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "prefix" {
  type        = string
  default     = "devex"
  description = "Prefix given to the generated pet name."
}

# The basic fixture with one resource renamed, and nothing else changed. It is
# the other half of a StateMv: moving the address in state has to leave a plan
# against this configuration empty, which it only does if the two fixtures
# agree on everything but the name.
resource "random_pet" "renamed" {
  prefix    = var.prefix
  length    = 2
  separator = "-"
}

resource "random_integer" "port" {
  min = 30000
  max = 32767
}

output "name" {
  value = random_pet.renamed.id
}

output "port" {
  value = random_integer.port.result
}
