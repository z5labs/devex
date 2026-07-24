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

# Nothing here touches the filesystem or a cloud API: the random provider's
# resources exist only in state, so a plan against state emitted by a previous
# apply is genuinely empty rather than "the file I made went missing".
resource "random_pet" "name" {
  prefix    = var.prefix
  length    = 2
  separator = "-"
}

resource "random_integer" "port" {
  min = 30000
  max = 32767
}

output "name" {
  value = random_pet.name.id
}

output "port" {
  value = random_integer.port.result
}
