import Contacts
import Foundation

struct ContactResult: Codable {
    let name: String?
    let organization: String?
    let phone_number: String
    let label: String?
}

struct ProbeResult: Codable {
    let authorized: Bool
    let authorization_status: String
    let message: String
}

func authorizationName(_ status: CNAuthorizationStatus) -> String {
    switch status {
    case .notDetermined:
        return "not_determined"
    case .restricted:
        return "restricted"
    case .denied:
        return "denied"
    case .authorized:
        return "authorized"
    @unknown default:
        return "unknown"
    }
}

func requestAccess(to store: CNContactStore) -> (Bool, CNAuthorizationStatus) {
    var status = CNContactStore.authorizationStatus(for: .contacts)
    if status == .notDetermined {
        let semaphore = DispatchSemaphore(value: 0)
        store.requestAccess(for: .contacts) { _, _ in
            semaphore.signal()
        }
        _ = semaphore.wait(timeout: .now() + 60)
        status = CNContactStore.authorizationStatus(for: .contacts)
    }
    return (status == .authorized, status)
}

func normalizedPhone(_ value: String) -> String {
    var normalized = String(value.filter { $0.isNumber })
    if normalized.hasPrefix("00") {
        normalized.removeFirst(2)
    }
    return normalized
}

func writeJSON<T: Encodable>(_ value: T) throws {
    let encoder = JSONEncoder()
    let data = try encoder.encode(value)
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data("\n".utf8))
}

let arguments = Array(CommandLine.arguments.dropFirst())
let trimmedQuery = (arguments.first ?? "")
    .trimmingCharacters(in: .whitespacesAndNewlines)

if arguments.first != "--probe" && trimmedQuery.isEmpty {
    try writeJSON([ContactResult]())
    exit(0)
}

let store = CNContactStore()
let (authorized, authorizationStatus) = requestAccess(to: store)

if arguments.first == "--probe" {
    try writeJSON(ProbeResult(
        authorized: authorized,
        authorization_status: authorizationName(authorizationStatus),
        message: authorized
            ? "Contacts.app is available for read-only lookup."
            : "Contacts access is not authorized."
    ))
    exit(authorized ? 0 : 2)
}

guard authorized else {
    FileHandle.standardError.write(Data("permission_denied\n".utf8))
    exit(2)
}

let query = trimmedQuery.lowercased()
let queryDigits = normalizedPhone(query)
let requestedLimit = arguments.count > 1 ? Int(arguments[1]) ?? 50 : 50
let limit = max(1, min(requestedLimit, 200))
let keys: [CNKeyDescriptor] = [
    CNContactFormatter.descriptorForRequiredKeys(for: .fullName),
    CNContactOrganizationNameKey as CNKeyDescriptor,
    CNContactPhoneNumbersKey as CNKeyDescriptor,
]
let request = CNContactFetchRequest(keysToFetch: keys)
request.unifyResults = true
var results: [ContactResult] = []

try store.enumerateContacts(with: request) { contact, stop in
    let formattedName = CNContactFormatter.string(from: contact, style: .fullName)
    let displayName = formattedName?.isEmpty == false ? formattedName : nil
    let organization = contact.organizationName.isEmpty ? nil : contact.organizationName

    for labeledPhone in contact.phoneNumbers {
        let phone = labeledPhone.value.stringValue
        let searchable = [displayName, organization, phone]
            .compactMap { $0 }
            .joined(separator: " ")
            .lowercased()
        let phoneMatches = !queryDigits.isEmpty && normalizedPhone(phone).contains(queryDigits)
        if !query.isEmpty && !searchable.contains(query) && !phoneMatches {
            continue
        }

        let label = labeledPhone.label.map { CNLabeledValue<CNPhoneNumber>.localizedString(forLabel: $0) }
        results.append(ContactResult(
            name: displayName ?? organization,
            organization: organization,
            phone_number: phone,
            label: label
        ))
        if results.count >= limit {
            stop.pointee = true
            break
        }
    }
}

try writeJSON(results)
