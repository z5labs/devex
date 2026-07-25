terraform {
  required_providers {
    random = {
        source = "hashicorp/random"
        version = "~> 3.6"
    }
  }
}

# Broken twice over, on purpose: the block below is unformatted, so `tofu fmt`
# rejects it, and the output references a resource that is never declared, so
# `tofu validate` rejects it too. A Ci.Check with both stages enabled has to
# report both failures — one round trip, not two.
resource "random_pet" "name" {
    length = 2
      separator   =   "-"
}

output "name" {
  value = random_pet.missing.id
}
