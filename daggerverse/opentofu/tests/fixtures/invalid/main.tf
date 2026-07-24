terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

resource "random_pet" "name" {
  length = 2
}

# random_pet.missing is never declared, so validate rejects the reference.
output "name" {
  value = random_pet.missing.id
}
