require_relative "test_helper"
require "securerandom"

class AuthTest < Minitest::Test
  include SiloTestHelper

  # Login mints a session credential. It used to return a JWT signed against a
  # server-wide secret, which could not be revoked, named or scoped -- see
  # docs/auth.md findings 3, 5 and 6. The response field is still "token"
  # holding a string, deliberately, so what a client does with it did not
  # change.
  def test_login_returns_a_session_credential
    c = SiloClient.new(silo_url)
    resp = c.login(silo_email, silo_password)
    assert resp.ok?, "Login failed: #{resp}"
    assert resp["token"], "Expected token in response"
    assert resp["token"].start_with?("silo_session_"),
      "Token should be a session credential, got #{resp["token"][0, 24]}..."
    refute resp["token"].include?("."), "A JWT came back; the credential lane is not mounted"
  end

  # The checksum is six base32 characters over everything before it, so a
  # truncated paste is refused as malformed before the database is touched.
  # It must not be mistaken for authentication: both answer 401.
  def test_a_truncated_credential_is_refused
    c = SiloClient.new(silo_url)
    token = c.login(silo_email, silo_password)["token"]

    bad = SiloClient.new(silo_url)
    bad.instance_variable_set(:@token, token[0...-4])
    resp = bad.request(:get, "/api/silo/v1/libraries")
    assert_equal 401, resp.status
  end

  def test_login_wrong_password
    c = SiloClient.new(silo_url)
    resp = c.post("/api/silo/v1/auth/login", { email: silo_email, password: "wrong" }, auth: false)
    assert_equal 401, resp.status
  end

  def test_login_missing_fields
    c = SiloClient.new(silo_url)

    resp = c.post("/api/silo/v1/auth/login", { email: silo_email }, auth: false)
    assert_equal 400, resp.status

    resp = c.post("/api/silo/v1/auth/login", { password: "whatever" }, auth: false)
    assert_equal 400, resp.status
  end

  def test_login_nonexistent_user
    c = SiloClient.new(silo_url)
    resp = c.post("/api/silo/v1/auth/login", { email: "nobody@example.com", password: "x" }, auth: false)
    assert_equal 401, resp.status
  end

  def test_protected_endpoints_reject_no_auth
    c = SiloClient.new(silo_url)

    resp = c.request(:get, "/api/silo/v1/libraries", auth: false)
    assert_equal 401, resp.status

    resp = c.request(:post, "/api/silo/v1/libraries", body: { name: "x" }, auth: false)
    assert_equal 401, resp.status
  end

  # Logging out over the wire, against the running binary. The Go tests measure
  # the contract; what this adds is the process -- the route is registered
  # outside the authenticated subrouter, on its own middleware lane, and a
  # mount that answers 404 in production would pass every router test.
  #
  # Only the self form is here. Signing the account out everywhere, or changing
  # its password, would break every other test in the run and the operator's
  # SILO_PASSWORD with it; both are covered in Go, against a database per test.
  def test_logging_out_discards_the_credential
    c = SiloClient.new(silo_url)
    c.login(silo_email, silo_password)
    assert c.list_libraries.ok?, "the credential did not work before logging out"

    resp = c.post("/api/silo/v1/auth/logout")
    assert resp.ok?, resp.to_s
    assert_equal 1, resp["revoked"]

    assert_equal 401, c.list_libraries.status, "the credential still works after logging out"
  end

  def test_protected_endpoints_reject_bad_token
    c = SiloClient.new(silo_url)
    c.instance_variable_set(:@token, "not.a.valid.jwt.token")

    resp = c.list_libraries
    assert_equal 401, resp.status
  end
end
