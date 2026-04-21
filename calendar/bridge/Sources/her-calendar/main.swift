// main.swift
// Entry point for the her-calendar CLI bridge.
//
// This binary reads one JSON command from stdin, executes it via EventKit,
// and writes one JSON response to stdout. Then it exits. No HTTP, no daemon,
// no state between invocations — just a simple stdin/stdout filter.

import Foundation

// MARK: - Main Entry Point

// Read JSON from stdin
let inputData = FileHandle.standardInput.readDataToEndOfFile()

// Decode the request
let decoder = JSONDecoder()
var response: Response? = nil
var exitCode = 0

do {
    let request = try decoder.decode(Request.self, from: inputData)

    // Execute the command (async)
    // We use a semaphore to block until the async work completes
    // (Swift's way of "wait for this async thing to finish" in sync context)
    let semaphore = DispatchSemaphore(value: 0)

    Task {
        do {
            response = try await handleCommand(request)
            exitCode = 0
        } catch let error as CommandError {
            // Calendar-side errors (event not found, invalid date, etc.)
            response = Response(
                ok: false,
                result: nil,
                error: "calendar_error",
                message: error.description
            )
            exitCode = 2
        } catch {
            // Bridge errors (permission denied, unexpected failures)
            response = Response(
                ok: false,
                result: nil,
                error: "bridge_error",
                message: "Bridge error: \(error.localizedDescription)"
            )
            exitCode = 1
        }
        semaphore.signal()
    }

    semaphore.wait()

} catch {
    // JSON decode errors
    response = Response(
        ok: false,
        result: nil,
        error: "bridge_error",
        message: "Bridge error: \(error.localizedDescription)"
    )
    exitCode = 1
}

// Write response and exit
if let response = response {
    writeResponse(response)
}
exit(Int32(exitCode))

// MARK: - Helper: Write Response

/// Write a Response struct to stdout as JSON.
func writeResponse(_ response: Response) {
    let encoder = JSONEncoder()
    encoder.outputFormatting = .prettyPrinted  // Make it readable for debugging

    do {
        let jsonData = try encoder.encode(response)
        if let jsonString = String(data: jsonData, encoding: .utf8) {
            print(jsonString)
        }
    } catch {
        // If we can't encode the response, write a minimal error JSON manually
        print("""
        {
          "ok": false,
          "error": "encoding_error",
          "message": "Failed to encode response: \(error.localizedDescription)"
        }
        """)
    }
}
