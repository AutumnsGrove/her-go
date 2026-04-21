// JSON.swift
// Data structures for the wire protocol (stdin/stdout communication with Go).
//
// In Swift, "Codable" is a protocol (like Go interfaces) that enables
// automatic JSON encoding/decoding. It's similar to Go's struct tags like
// `json:"field_name"`, but Swift handles it through the type system.

import Foundation

// MARK: - Request Types

/// The command sent from Go via stdin.
/// Swift struct syntax: "struct Name: Protocol { properties }"
/// The ": Codable" part means this can be converted to/from JSON automatically.
struct Request: Codable {
    let command: String       // "list", "create", "update", "delete"
    let calendar: String      // Name of the calendar in Apple Calendar
    let args: RequestArgs     // Command-specific arguments
}

/// Command-specific arguments. Swift enums with associated values are like
/// Rust/TypeScript tagged unions — one variant is active at a time.
enum RequestArgs: Codable {
    case list(start: String, end: String)
    case create(events: [EventInput])
    case update(id: String, event: EventInput)
    case delete(id: String)
    case listCalendars  // No args - just discover available calendars

    // CodingKeys tells Swift how to map between Swift property names and JSON keys.
    // This is like Go's `json:"field_name"` struct tags.
    enum CodingKeys: String, CodingKey {
        case start, end, events, id, event
    }

    // Custom encoding/decoding logic (since we're not using a simple struct).
    // Swift requires explicit code for enums with associated values.
    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)

        // Try to decode each variant by checking which keys are present
        if container.contains(.start) && container.contains(.end) {
            let start = try container.decode(String.self, forKey: .start)
            let end = try container.decode(String.self, forKey: .end)
            self = .list(start: start, end: end)
        } else if container.contains(.events) {
            let events = try container.decode([EventInput].self, forKey: .events)
            self = .create(events: events)
        } else if container.contains(.id) && container.contains(.event) {
            let id = try container.decode(String.self, forKey: .id)
            let event = try container.decode(EventInput.self, forKey: .event)
            self = .update(id: id, event: event)
        } else if container.contains(.id) {
            let id = try container.decode(String.self, forKey: .id)
            self = .delete(id: id)
        } else {
            // Empty args object → list_calendars
            self = .listCalendars
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .list(let start, let end):
            try container.encode(start, forKey: .start)
            try container.encode(end, forKey: .end)
        case .create(let events):
            try container.encode(events, forKey: .events)
        case .update(let id, let event):
            try container.encode(id, forKey: .id)
            try container.encode(event, forKey: .event)
        case .delete(let id):
            try container.encode(id, forKey: .id)
        case .listCalendars:
            break  // No args to encode
        }
    }
}

/// Event data coming from Go (for create/update).
/// The "?" suffix means optional — Swift's version of nullable values.
/// This is different from Go, where you'd use a pointer (*string) or empty string.
struct EventInput: Codable {
    let title: String?
    let start: String?          // ISO 8601 timestamp
    let end: String?            // ISO 8601 timestamp
    let location: String?
    let notes: String?
    let calendar: String?       // NEW: optional target calendar for this event
}

// MARK: - Response Types

/// Response sent back to Go via stdout.
struct Response: Codable {
    let ok: Bool                       // true = success, false = error
    let result: ResponseResult?        // Present when ok=true
    let error: String?                 // Error code when ok=false
    let message: String?               // Human-readable detail when ok=false
}

/// Result data varies by command.
enum ResponseResult: Codable {
    case list(events: [EventOutput])
    case create(events: [EventID])
    case update(id: String)
    case delete(deleted: Bool)
    case listCalendars(calendars: [String])

    enum CodingKeys: String, CodingKey {
        case events, id, deleted, calendars
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)

        if let events = try? container.decode([EventOutput].self, forKey: .events) {
            self = .list(events: events)
        } else if let events = try? container.decode([EventID].self, forKey: .events) {
            self = .create(events: events)
        } else if let calendars = try? container.decode([String].self, forKey: .calendars) {
            self = .listCalendars(calendars: calendars)
        } else if let id = try? container.decode(String.self, forKey: .id) {
            self = .update(id: id)
        } else if let deleted = try? container.decode(Bool.self, forKey: .deleted) {
            self = .delete(deleted: deleted)
        } else {
            throw DecodingError.dataCorrupted(
                DecodingError.Context(codingPath: decoder.codingPath,
                                     debugDescription: "Unknown response result format")
            )
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .list(let events):
            try container.encode(events, forKey: .events)
        case .create(let events):
            try container.encode(events, forKey: .events)
        case .listCalendars(let calendars):
            try container.encode(calendars, forKey: .calendars)
        case .update(let id):
            try container.encode(id, forKey: .id)
        case .delete(let deleted):
            try container.encode(deleted, forKey: .deleted)
        }
    }
}

/// Event data sent back to Go (from list command).
struct EventOutput: Codable {
    let id: String
    let title: String
    let start: String           // ISO 8601 timestamp
    let end: String             // ISO 8601 timestamp
    let location: String?       // Optional fields use "?" suffix
    let notes: String?
    let calendar: String        // NEW: which calendar this event is from
}

/// Just the event ID (returned by create command).
struct EventID: Codable {
    let id: String
}
