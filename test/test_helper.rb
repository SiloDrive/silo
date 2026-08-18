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

  # Creates a repo and ensures it's cleaned up after the test.
  def create_test_repo(name = "test-#{SecureRandom.hex(4)}")
    resp = client.create_repo(name)
    assert resp.ok?, "Failed to create repo: #{resp}"
    repo_id = resp["id"]
    @test_repos ||= []
    @test_repos << repo_id
    repo_id
  end

  def teardown
    (@test_repos || []).each do |repo_id|
      client.delete_repo(repo_id)
    end
  end
end
