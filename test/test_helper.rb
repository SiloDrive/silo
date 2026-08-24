require "minitest/autorun"
require_relative "silo_client"

# Configuration via environment variables:
#   SILO_URL      - server base URL (default: http://localhost:8082)
#   SILO_EMAIL    - test user email (default: admin@example.com)
#   SILO_PASSWORD - test user password (required)

module SiloTestHelper
  def silo_url
    ENV.fetch("SILO_URL", "http://localhost:8082")
  end

  def silo_email
    ENV.fetch("SILO_EMAIL", "admin@example.com")
  end

  def silo_password
    ENV["SILO_PASSWORD"] || raise("SILO_PASSWORD env var is required")
  end

  # Returns a logged-in client. Memoized per test instance.
  def client
    @client ||= begin
      c = SiloClient.new(silo_url)
      c.login(silo_email, silo_password)
      c
    end
  end

  # Returns an unauthenticated client.
  def anon_client
    @anon_client ||= SiloClient.new(silo_url)
  end

  # Creates a library and ensures it's cleaned up after the test.
  def create_test_library(name = "test-#{SecureRandom.hex(4)}")
    resp = client.create_library(name)
    assert resp.ok?, "Failed to create library: #{resp}"
    library_id = resp["id"]
    @test_libraries ||= []
    @test_libraries << library_id
    library_id
  end

  def teardown
    (@test_libraries || []).each do |library_id|
      client.delete_library(library_id)
    end
  end
end
