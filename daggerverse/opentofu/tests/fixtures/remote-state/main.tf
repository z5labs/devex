terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # A partial configuration: the block names the backend and nothing else, so
  # every setting arrives at init time and one root module can be pointed at
  # whichever bucket the caller stood up.
  backend "s3" {}
}

# Same shape as the basic fixture, and for the same reason: the random
# provider's resources exist only in state, so what the backend holds after an
# apply is the whole truth about them.
resource "random_pet" "name" {
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
