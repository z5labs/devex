terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "token" {
  type        = string
  sensitive   = true
  description = "A credential supplied through WithSecretVar."
}

resource "random_pet" "name" {
  length = 2
}

# The token's length proves it reached tofu; the value itself never enters a
# resource attribute, so it cannot end up in state, in the saved plan, or in
# the JSON rendering of either.
output "token_length" {
  value     = length(var.token)
  sensitive = true
}
