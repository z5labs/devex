const std = @import("std");

pub fn main() !void {
    const stdout = std.io.getStdOut().writer();
    try stdout.print("single\n", .{});
}

test "single arithmetic" {
    try std.testing.expectEqual(@as(i32, 2), 1 + 1);
}
