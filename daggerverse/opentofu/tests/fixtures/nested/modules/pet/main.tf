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
  description = "Prefix given to the generated pet name."
}

resource "random_pet" "name" {
  prefix    = var.prefix
  length    = 2
  separator = "-"
}

output "name" {
  value = random_pet.name.id
}
