const std = @import("std");

pub fn main() !void {
    const stdout = std.io.getStdOut().writer();
    // Deliberate type error, reachable from main() so Zig cannot lazily skip
    // it: a string literal ([]const u8) bound to a u32. The syntax is valid,
    // so `zig fmt --check` passes; the code does not type-check, so `zig build`
    // fails. This is the exact false-green failure mode from issue #161 — a
    // project whose only failure is caught by the build, not by fmt.
    const value: u32 = "definitely not a u32";
    try stdout.print("{d}\n", .{value});
}
