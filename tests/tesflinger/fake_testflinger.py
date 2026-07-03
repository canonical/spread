import json
import os
import sys

from datetime import datetime, timezone
from flask import Flask, jsonify, request, abort

app = Flask(__name__)

count_calls_result = 0
current_job = ""

# Define tus rutas y respuestas mock
@app.route('/v1/job', methods=['POST'])
def mock_job():
    global count_calls_result 
    global current_job
    count_calls_result = 0
    
    data = request.get_json()
    filename = "job_{}.data".format(current_job)
    with open(filename, "w") as file:
        json.dump(data, file)
    
    return jsonify({"job_id": current_job})


# Basic scenario
@app.route('/v1/result/00000000-0000-0000-0000-000000000001', methods=['GET'])
def mock_result_basic():
    global count_calls_result
    count_calls_result += 1

    if count_calls_result < 2:
        return jsonify({"job_state":"waiting"})
    elif count_calls_result < 5:
        return jsonify({"job_state":"provision"})
    elif count_calls_result < 6:
        return jsonify({"job_state":"test"})        
    elif count_calls_result < 7:
        return jsonify({"job_state":"reserve"})
    elif count_calls_result < 8:
        return jsonify({"job_state":"allocate"})        
    else:
        return jsonify({"device_info":{"device_ip":"127.0.0.1"},"job_state":"allocated"})


@app.route('/v1/job/00000000-0000-0000-0000-000000000001/action', methods=['POST'])
def mock_action_basic():
    job_id = "00000000-0000-0000-0000-000000000001"
    data = request.get_json()
    filename = "action_{}.data".format(job_id)
    with open(filename, "w") as file:
        json.dump(data, file)

    return jsonify({})


# Error scenario
@app.route('/v1/result/00000000-0000-0000-0000-000000000002', methods=['GET'])
def mock_result_error():
    global count_calls_result
    count_calls_result += 1

    if count_calls_result < 3:
        return jsonify({"job_state":"waiting"})
    else:
        return jsonify({"job_state":"cancelled", 
            "allocate_output":"test_allocate", 
            "provision_output":"test_provision", 
            "reserve_output":"test_reserve", 
            "setup_output":"test_setup"})


# Complete scenario
@app.route('/v1/result/00000000-0000-0000-0000-000000000003', methods=['GET'])
def mock_result_complete():
    global count_calls_result
    count_calls_result += 1

    if count_calls_result < 3:
        return jsonify({"job_state":"waiting"})
    else:
        return jsonify({"job_state":"complete"})


# Garbage collection scenario
@app.route('/v1/job/search', methods=['GET'])
def mock_result_completed():
    local_time = datetime.now().strftime('%Y-%m-%dT%H:%M:%SZ')

    tag = request.args.get('tags')
    state = request.args.get('state')
    if tag != "spread" or state != "active":
        return jsonify([])

    # This list contains:
    # 1 old active job (has to be removed)
    # 1 recent active job (hasn't to be removed) 
    # 1 old completed job (hasn't to be removed)
    # 1 old cancelled job (hasn't to be removed)
    return jsonify([
        {"created_at":"2024-02-09T02:58:49Z","job_id":"00000000-0000-0000-0000-000000000004","job_state":"waiting"},
        {"created_at":local_time,"job_id":"00000000-0000-0000-0000-000000000005","job_state":"waiting"},
        {"created_at":"2024-02-09T02:58:49Z","job_id":"00000000-0000-0000-0000-000000000006","job_state":"complete"},
        {"created_at":"2024-02-09T02:58:49Z","job_id":"00000000-0000-0000-0000-000000000007","job_state":"cancelled"}])


@app.route('/v1/job/00000000-0000-0000-0000-000000000004/action', methods=['POST'])
def mock_action_delete():
    job_id = "00000000-0000-0000-0000-000000000004"
    data = request.get_json()
    filename = "action_{}.data".format(job_id)
    with open(filename, "w") as file:
        json.dump(data, file)

    return jsonify({})

@app.route('/v1/job/00000000-0000-0000-0000-000000000004', methods=['GET'])
def mock_job_tags_4():
    return jsonify({
      "allocate_data": {},
      "allocation_timeout": 0,
      "firmware_update_data": {},
      "global_timeout": 0,
      "job_id": "00000000-0000-0000-0000-000000000004",
      "job_queue": "queue",
      "name": "name",
      "output_timeout": 0,
      "parent_job_id": "job",
      "provision_data": {},
      "reserve_data": {},
      "tags": ["spread", "halt-timeout=4h"],
      "test_data": {}
      })


@app.route('/v1/job/00000000-0000-0000-0000-000000000005/action', methods=['POST'])
def mock_action_error_5():
    abort(500)

@app.route('/v1/job/00000000-0000-0000-0000-000000000005', methods=['GET'])
def mock_job_tags_5():
    return jsonify({
      "allocate_data": {},
      "allocation_timeout": 0,
      "firmware_update_data": {},
      "global_timeout": 0,
      "job_id": "00000000-0000-0000-0000-000000000005",
      "job_queue": "queue",
      "name": "name",
      "output_timeout": 0,
      "parent_job_id": "job",
      "provision_data": {},
      "reserve_data": {},
      "tags": ["spread", "halt-timeout=4h"],
      "test_data": {}
    })

@app.route('/v1/job/00000000-0000-0000-0000-000000000006/action', methods=['POST'])
def mock_action_error_6():
    abort(500)

@app.route('/v1/job/00000000-0000-0000-0000-000000000006', methods=['GET'])
def mock_job_error_6():
    abort(500)

@app.route('/v1/job/00000000-0000-0000-0000-000000000007/action', methods=['POST'])
def mock_action_error_7():
    abort(500)

@app.route('/v1/job/00000000-0000-0000-0000-000000000007', methods=['GET'])
def mock_job_error_7():
    abort(500)


def main(argv):
    if len(argv) < 2:
        print("fake_testflinger: Missing job id")
        sys.exit(1)

    global current_job
    current_job = argv[1]
    print("fake_testflinger: starting mock for job {}".format(current_job))
    app.run(port=5005)


if __name__ == '__main__':
    main(sys.argv)
