// swift-tools-version: 5.9
// The Swift Package Manager manifest file.
// This is like go.mod — it declares what we're building and what it depends on.

import PackageDescription

let package = Package(
    // Package name (similar to Go module name)
    name: "her-calendar",

    // Minimum macOS version required (using macOS 14+ for .fullAccess permission check)
    platforms: [
        .macOS(.v14)
    ],

    // Products: what this package produces when built
    // We're building one executable named "her-calendar"
    products: [
        .executable(
            name: "her-calendar",
            targets: ["her-calendar"]
        )
    ],

    // Dependencies: external packages we need (none for this project)
    // EventKit is a system framework, so it doesn't need to be listed here
    dependencies: [
    ],

    // Targets: the actual code modules
    // This is like defining what gets compiled
    targets: [
        .executableTarget(
            name: "her-calendar",
            dependencies: [],
            // Swift can automatically link system frameworks
            // We'll import EventKit in the source files
            path: "Sources/her-calendar"
        )
    ]
)
