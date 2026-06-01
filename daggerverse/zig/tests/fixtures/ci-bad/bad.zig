const testing = @import("std").testing;
test "always fails" {
try testing.expect(false);
}
