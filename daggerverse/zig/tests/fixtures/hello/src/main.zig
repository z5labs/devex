const std = @import("std");

pub fn main() !void {
    const stdout = std.io.getStdOut().writer();
    try stdout.print("hello\n", .{});
}

test "hello greets" {
    try std.testing.expect(true);
}
