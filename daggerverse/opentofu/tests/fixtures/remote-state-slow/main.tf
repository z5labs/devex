terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  backend "s3" {}
}

variable "delay_seconds" {
  type        = number
  default     = 12
  description = "How long an apply holds the state lock open."
}

# Holds the apply — and with it the state lock — open long enough for a second,
# concurrent apply to reach the backend while this one still owns it.
# terraform_data is built into tofu, so the delay costs no extra provider
# download, and with no triggers_replace it is created once and never again:
# an apply that observed the first one's state adds nothing.
resource "terraform_data" "delay" {
  provisioner "local-exec" {
    command = "sleep ${var.delay_seconds}"
  }
}

resource "random_pet" "name" {
  length    = 2
  separator = "-"
}

output "name" {
  value = random_pet.name.id
}
