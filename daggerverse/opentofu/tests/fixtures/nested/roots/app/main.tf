# A root module that sits inside a larger tree and uses a reusable module by a
# relative path reaching outside its own directory — the layout a repository
# with modules/ beside its root modules has. Bound alone, ../../modules/pet is
# outside what tofu was handed and init cannot find it.
terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

module "pet" {
  source = "../../modules/pet"
  prefix = "nested"
}

output "name" {
  value = module.pet.name
}
