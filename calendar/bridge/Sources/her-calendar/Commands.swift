// Commands.swift
// EventKit bridge logic — the 4 commands (list, create, update, delete).
//
// EventKit is Apple's calendar framework. Think of it like a specialized database:
// - EKEventStore = the connection to the calendar database
// - EKCalendar = a named calendar (like "Work" or "Personal")
// - EKEvent = a single calendar event with title, time, location, etc.

import Foundation
import EventKit

// MARK: - Command Handler

/// Main command dispatcher. Routes incoming requests to the appropriate handler.
/// Swift uses "throws" for functions that can error (similar to Go's error return value).
/// The "async" keyword means this function can await asynchronous operations (like requesting permissions).
func handleCommand(_ request: Request) async throws -> Response {
    let store = EKEventStore()

    // Request calendar access if not already granted
    // In macOS 14+, we must explicitly request permission using async/await
    let status = EKEventStore.authorizationStatus(for: .event)

    if status != .fullAccess {
        // Request full access permission (this will trigger the macOS permission prompt)
        // The "await" keyword means we wait for the async operation to complete
        let granted = try await store.requestFullAccessToEvents()

        guard granted else {
            return Response(
                ok: false,
                result: nil,
                error: "permission_denied",
                message: "Calendar permission was denied. Grant permission in System Settings → Privacy & Security → Calendars."
            )
        }
    }

    // Dispatch to the appropriate command handler based on args variant
    // Different commands need different calendar handling:
    // - list_calendars: discovers available calendars (no calendar param needed)
    // - list: uses comma-separated names or "*" wildcard
    // - create: uses default calendar (request.calendar) + per-event overrides
    // - update/delete: uses "*" wildcard to search all calendars
    let result: ResponseResult
    switch request.args {
    case .listCalendars:
        result = listCalendars(in: store)

    case .list(let start, let end):
        // Build calendar list from comma-separated names or wildcard
        let calendars: [EKCalendar]
        if request.calendar == "*" {
            calendars = store.calendars(for: .event)
        } else {
            let names = request.calendar.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
            calendars = names.compactMap { name in
                findCalendar(named: name, in: store)
            }
            guard !calendars.isEmpty else {
                return Response(
                    ok: false,
                    result: nil,
                    error: "no_calendars_found",
                    message: "None of the requested calendars were found: \(request.calendar)"
                )
            }
        }
        result = try listEvents(in: calendars, start: start, end: end, store: store)

    case .create(let events):
        // Find default calendar for events without explicit calendar
        guard let defaultCalendar = findCalendar(named: request.calendar, in: store) else {
            return Response(
                ok: false,
                result: nil,
                error: "default_calendar_not_found",
                message: "Default calendar '\(request.calendar)' not found"
            )
        }
        result = try createEvents(events, defaultCalendar: defaultCalendar, store: store)

    case .update(let id, let event):
        result = try updateEvent(id: id, with: event, calendarName: request.calendar, store: store)

    case .delete(let id):
        result = try deleteEvent(id: id, calendarName: request.calendar, store: store)
    }

    return Response(ok: true, result: result, error: nil, message: nil)
}

// MARK: - Helper: Find Calendar

/// Find a calendar by name in the event store.
/// Swift functions can return optionals (Type?) to indicate "might not exist".
/// This is like Go's `value, ok := map[key]` pattern, but baked into the type system.
func findCalendar(named name: String, in store: EKEventStore) -> EKCalendar? {
    // Get all calendars from the store
    let calendars = store.calendars(for: .event)

    // Find the one matching our name (case-insensitive)
    return calendars.first { $0.title.lowercased() == name.lowercased() }
}

// MARK: - Command: List Calendars

/// List all available calendars that the user can add events to.
/// This helps the agent discover which calendars exist without hardcoding names.
func listCalendars(in store: EKEventStore) -> ResponseResult {
    // Get all event calendars from the store
    let calendars = store.calendars(for: .event)

    // Extract calendar names (titles)
    let names = calendars.map { $0.title }

    return .listCalendars(calendars: names)
}

// MARK: - Command: List Events

/// List events in a date range from multiple calendars.
/// Returns a unified timeline with each event tagged with its source calendar.
func listEvents(in calendars: [EKCalendar], start: String, end: String, store: EKEventStore) throws -> ResponseResult {
    // Parse ISO 8601 timestamps (with or without fractional seconds)
    guard let startDate = parseISO8601(start) else {
        throw CommandError.invalidDate("Invalid start date: \(start)")
    }
    guard let endDate = parseISO8601(end) else {
        throw CommandError.invalidDate("Invalid end date: \(end)")
    }

    // Build a predicate (query filter) for the date range
    // EventKit natively supports multiple calendars in one query
    let predicate = store.predicateForEvents(
        withStart: startDate,
        end: endDate,
        calendars: calendars
    )

    // Fetch events matching the predicate
    let ekEvents = store.events(matching: predicate)

    // Convert EKEvent objects to our JSON-friendly EventOutput structs
    // Include the calendar name so the agent knows which calendar each event is from
    let events = ekEvents.map { event in
        EventOutput(
            id: event.eventIdentifier,
            title: event.title ?? "(No Title)",
            start: formatISO8601(event.startDate),
            end: formatISO8601(event.endDate),
            location: event.location,
            notes: event.notes,
            calendar: event.calendar.title
        )
    }

    return .list(events: events)
}

// MARK: - Command: Create Events

/// Create one or more events (atomic per call — all or nothing).
/// Each event can optionally specify its target calendar; otherwise uses defaultCalendar.
func createEvents(_ inputs: [EventInput], defaultCalendar: EKCalendar, store: EKEventStore) throws -> ResponseResult {
    var createdEvents: [EKEvent] = []
    var eventIDs: [EventID] = []

    // Create each event
    for input in inputs {
        // Validate required fields
        guard let title = input.title, !title.isEmpty else {
            // Rollback on error: delete anything we already created
            rollbackEvents(createdEvents, in: store)
            throw CommandError.missingField("Event title is required")
        }
        guard let startStr = input.start, let startDate = parseISO8601(startStr) else {
            rollbackEvents(createdEvents, in: store)
            throw CommandError.invalidDate("Invalid or missing start date")
        }
        guard let endStr = input.end, let endDate = parseISO8601(endStr) else {
            rollbackEvents(createdEvents, in: store)
            throw CommandError.invalidDate("Invalid or missing end date")
        }

        // Determine which calendar to use for this event
        let targetCalendar: EKCalendar
        if let calendarName = input.calendar {
            // Event specifies a calendar — find it
            guard let cal = findCalendar(named: calendarName, in: store) else {
                rollbackEvents(createdEvents, in: store)
                throw CommandError.calendarNotFound("Calendar '\(calendarName)' not found for event '\(title)'")
            }
            targetCalendar = cal
        } else {
            // Use default calendar
            targetCalendar = defaultCalendar
        }

        // Create the EKEvent
        let event = EKEvent(eventStore: store)
        event.calendar = targetCalendar
        event.title = title
        event.startDate = startDate
        event.endDate = endDate
        event.location = input.location
        event.notes = input.notes

        // Save to the store
        do {
            try store.save(event, span: .thisEvent, commit: false)
            createdEvents.append(event)
        } catch {
            // Rollback on save failure
            rollbackEvents(createdEvents, in: store)
            throw CommandError.saveFailed("Failed to save event: \(error.localizedDescription)")
        }
    }

    // Commit all changes at once (atomic batch)
    do {
        try store.commit()
    } catch {
        rollbackEvents(createdEvents, in: store)
        throw CommandError.saveFailed("Failed to commit events: \(error.localizedDescription)")
    }

    // Return the event IDs
    eventIDs = createdEvents.map { EventID(id: $0.eventIdentifier) }
    return .create(events: eventIDs)
}

/// Rollback helper: delete all events in the list.
func rollbackEvents(_ events: [EKEvent], in store: EKEventStore) {
    for event in events {
        try? store.remove(event, span: .thisEvent, commit: false)
    }
    try? store.commit()
}

// MARK: - Command: Update Event

/// Update an existing event by ID.
/// If calendarName is "*", searches across all calendars (eventIdentifier is globally unique).
/// Otherwise, verifies the event is in the specified calendar.
func updateEvent(id: String, with input: EventInput, calendarName: String, store: EKEventStore) throws -> ResponseResult {
    // Find the event by ID (eventIdentifier is globally unique)
    guard let event = store.event(withIdentifier: id) else {
        throw CommandError.eventNotFound("Event with ID '\(id)' not found")
    }

    // If not wildcard, verify the event is in the requested calendar
    if calendarName != "*" {
        guard let requestedCalendar = findCalendar(named: calendarName, in: store) else {
            throw CommandError.calendarNotFound("Calendar '\(calendarName)' not found")
        }
        guard event.calendar.calendarIdentifier == requestedCalendar.calendarIdentifier else {
            throw CommandError.eventNotFound("Event '\(id)' exists but not in calendar '\(calendarName)'")
        }
    }

    // Update only the fields that are provided (nil = don't change)
    if let title = input.title {
        event.title = title
    }
    if let startStr = input.start, let startDate = parseISO8601(startStr) {
        event.startDate = startDate
    }
    if let endStr = input.end, let endDate = parseISO8601(endStr) {
        event.endDate = endDate
    }
    if let location = input.location {
        event.location = location
    }
    if let notes = input.notes {
        event.notes = notes
    }

    // Save changes
    do {
        try store.save(event, span: .thisEvent)
    } catch {
        throw CommandError.saveFailed("Failed to update event: \(error.localizedDescription)")
    }

    return .update(id: event.eventIdentifier)
}

// MARK: - Command: Delete Event

/// Delete an event by ID.
/// If calendarName is "*", searches across all calendars (eventIdentifier is globally unique).
/// Otherwise, verifies the event is in the specified calendar.
func deleteEvent(id: String, calendarName: String, store: EKEventStore) throws -> ResponseResult {
    // Find the event (eventIdentifier is globally unique)
    guard let event = store.event(withIdentifier: id) else {
        throw CommandError.eventNotFound("Event with ID '\(id)' not found")
    }

    // If not wildcard, verify the event is in the requested calendar
    if calendarName != "*" {
        guard let requestedCalendar = findCalendar(named: calendarName, in: store) else {
            throw CommandError.calendarNotFound("Calendar '\(calendarName)' not found")
        }
        guard event.calendar.calendarIdentifier == requestedCalendar.calendarIdentifier else {
            throw CommandError.eventNotFound("Event '\(id)' exists but not in calendar '\(calendarName)'")
        }
    }

    // Delete it
    do {
        try store.remove(event, span: .thisEvent)
    } catch {
        throw CommandError.deleteFailed("Failed to delete event: \(error.localizedDescription)")
    }

    return .delete(deleted: true)
}

// MARK: - Date Helpers

/// Parse an ISO 8601 date string (with or without fractional seconds).
/// Tries multiple format options since Go and Swift may format differently.
func parseISO8601(_ string: String) -> Date? {
    let formatter = ISO8601DateFormatter()

    // Try with fractional seconds first
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = formatter.date(from: string) {
        return date
    }

    // Try without fractional seconds
    formatter.formatOptions = [.withInternetDateTime]
    if let date = formatter.date(from: string) {
        return date
    }

    return nil
}

/// Format a Date as ISO 8601 with timezone offset (no fractional seconds for simplicity).
func formatISO8601(_ date: Date) -> String {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime]
    return formatter.string(from: date)
}

// MARK: - Error Types

/// Custom error enum for command failures.
/// Swift enums can have associated values (like Rust enums or TypeScript tagged unions).
enum CommandError: Error, CustomStringConvertible {
    case invalidDate(String)
    case missingField(String)
    case calendarNotFound(String)
    case eventNotFound(String)
    case saveFailed(String)
    case deleteFailed(String)

    var description: String {
        switch self {
        case .invalidDate(let msg): return msg
        case .missingField(let msg): return msg
        case .calendarNotFound(let msg): return msg
        case .eventNotFound(let msg): return msg
        case .saveFailed(let msg): return msg
        case .deleteFailed(let msg): return msg
        }
    }
}
