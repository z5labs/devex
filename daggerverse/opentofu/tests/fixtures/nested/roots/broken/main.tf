# A sibling of roots/app that uses the same module without its required
# prefix. It exists so a Validate through path proves something: were path
# ignored, both roots would validate as the empty directory above them.
module "pet" {
  source = "../../modules/pet"
}
