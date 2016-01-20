import subprocess

import jinja2
import unittest
import os
import shutil
import json
import signal
import sys
import time
import yaml
from datetime import datetime, timedelta


class Proc(object):
    """
    Slim wrapper on subprocess.Popen that redirects
    both stdout and stderr to a file on disk and makes
    sure to stop the process and close the output file when
    the object gets collected.
    """

    def __init__(self, args, outputfile):
        self.args = args
        self.output = open(outputfile, "wb")

    def start(self):
        self.stdin_read, self.stdin_write = os.pipe()

        if sys.platform.startswith("win"):
            self.proc = subprocess.Popen(
                self.args,
                stdin=self.stdin_read,
                stdout=self.output,
                stderr=subprocess.STDOUT,
                bufsize=0,
                creationflags=subprocess.CREATE_NEW_PROCESS_GROUP)
        else:
            self.proc = subprocess.Popen(
                self.args,
                stdin=self.stdin_read,
                stdout=self.output,
                stderr=subprocess.STDOUT,
                bufsize=0,
            )
        return self.proc

    def kill(self):
        if sys.platform.startswith("win"):
            # proc.terminate on Windows does not initiate a graceful shutdown
            # through the processes signal handlers it just kills it hard. So
            # this sends a SIGBREAK. You cannot sends a SIGINT (CTRL_C_EVENT)
            # to a process group in Windows, otherwise Ctrl+C would be
            # sent.
            self.proc.send_signal(signal.CTRL_BREAK_EVENT)
        else:
            self.proc.terminate()

    def wait(self):
        return self.proc.wait()

    def kill_and_wait(self):
        self.kill()
        os.close(self.stdin_write)
        return self.proc.wait()

    def __del__(self):
        # Ensure the process is stopped.
        try:
            self.proc.terminate()
            self.proc.kill()
        except:
            pass
        # Ensure the output is closed.
        try:
            self.output.close()
        except:
            pass


class TestCase(unittest.TestCase):

    @classmethod
    def setUpClass(self):
        self.beat_name = "beat"
        self.build_path = "../../build/system-tests/"
        self.beat_path = "../../" + self.beat_name + ".test"

    def run_beat(self, cmd=None,
                 config=None,
                 output=None,
                 extra_args=[]):
        """
        Executes beat
        Waits for the process to finish before returning to
        the caller.
        """

        # Init defaults
        if cmd is None:
            cmd = self.beat_path

        if config is None:
            config = self.beat_name + ".yml"

        if output is None:
            output = self.beat_name + ".log"

        args = [cmd]

        args.extend(["-e",
                     "-c", os.path.join(self.working_dir, config),
                     "-systemTest",
                     "-v",
                     "-d", "*",
                     "-test.coverprofile", os.path.join(
                         self.working_dir, "coverage.cov")
                     ])

        if extra_args:
            args.extend(extra_args)

        with open(os.path.join(self.working_dir, output), "wb") as outputfile:
            proc = subprocess.Popen(args,
                                    stdout=outputfile,
                                    stderr=subprocess.STDOUT)
            return proc.wait()

    def start_beat(self,
                   cmd=None,
                   config=None,
                   output=None,
                   extra_args=[]):
        """
        Starts beat and returns the process handle. The
        caller is responsible for stopping / waiting for the
        Proc instance.
        """

        # Init defaults
        if cmd is None:
            cmd = self.beat_path

        if config is None:
            config = self.beat_name + ".yml"

        if output is None:
            output = self.beat_name + ".log"

        args = [cmd,
                "-e",
                "-c", os.path.join(self.working_dir, config),
                "-systemTest",
                "-v",
                "-d", "*",
                "-test.coverprofile", os.path.join(
                    self.working_dir, "coverage.cov")
                ]

        if extra_args:
            args.extend(extra_args)

        proc = Proc(args, os.path.join(self.working_dir, output))
        proc.start()
        return proc

    def render_config_template(self, template=None,
                               output=None, **kargs):

        # Init defaults
        if template is None:
            template = self.beat_name + ".yml.j2"

        if output is None:
            output = self.beat_name + ".yml"

        template = self.template_env.get_template(template)

        kargs["beat"] = self
        output_str = template.render(**kargs)
        with open(os.path.join(self.working_dir, output), "wb") as f:
            f.write(output_str)

    def read_output(self, output_file=None):

        # Init defaults
        if output_file is None:
            output_file = "output/" + self.beat_name

        jsons = []
        with open(os.path.join(self.working_dir, output_file), "r") as f:
            for line in f:
                jsons.append(self.flatten_object(json.loads(line),
                                                 []))
        self.all_have_fields(jsons, ["@timestamp", "type",
                                     "beat.name", "beat.hostname",
                                     "count"])
        return jsons

    def copy_files(self, files, source_dir="files/"):
        for file_ in files:
            shutil.copy(os.path.join(source_dir, file_),
                        self.working_dir)

    def setUp(self):

        self.template_env = jinja2.Environment(
            loader=jinja2.FileSystemLoader("config")
        )

        # create working dir
        self.working_dir = os.path.join(self.build_path + "run", self.id())
        if os.path.exists(self.working_dir):
            shutil.rmtree(self.working_dir)
        os.makedirs(self.working_dir)

        try:
            # update the last_run link
            if os.path.islink(self.build_path + "last_run"):
                os.unlink(self.build_path + "last_run")
            os.symlink(self.build_path + "run/{}".format(self.id()),
                       self.build_path + "last_run")
        except:
            # symlink is best effort and can fail when
            # running tests in parallel
            pass

    def wait_until(self, cond, max_timeout=10, poll_interval=0.1, name="cond"):
        """
        Waits until the cond function returns true,
        or until the max_timeout is reached. Calls the cond
        function every poll_interval seconds.

        If the max_timeout is reached before cond() returns
        true, an exception is raised.
        """
        start = datetime.now()
        while not cond():
            if datetime.now() - start > timedelta(seconds=max_timeout):
                raise Exception("Timeout waiting for '{}' to be true. "
                                .format(name) +
                                "Waited {} seconds.".format(max_timeout))
            time.sleep(poll_interval)

    def log_contains(self, msg, logfile=None):
        """
        Returns true if the give logfile contains the given message.
        Note that the msg must be present in a single line.
        """

        # Init defaults
        if logfile is None:
            logfile = self.beat_name + ".log"

        try:
            with open(os.path.join(self.working_dir, logfile), "r") as f:
                for line in f:
                    if line.find(msg) >= 0:
                        return True
                return False
        except IOError:
            return False

    def output_has(self, lines, output_file=None):
        """
        Returns true if the output has a given number of lines.
        """

        # Init defaults
        if output_file is None:
            output_file = "output/" + self.beat_name

        try:
            with open(os.path.join(self.working_dir, output_file), "r") as f:
                return len([1 for line in f]) == lines
        except IOError:
            return False

    def all_have_fields(self, objs, fields):
        """
        Checks that the given list of output objects have
        all the given fields.
        Raises Exception if not true.
        """
        for field in fields:
            if not all([field in o for o in objs]):
                raise Exception("Not all objects have a '{}' field"
                                .format(field))

    def all_have_only_fields(self, objs, fields):
        """
        Checks if the given list of output objects have all
        and only the given fields.
        Raises Exception if not true.
        """
        self.all_have_fields(objs, fields)
        self.all_fields_are_expected(objs, fields)

    def all_fields_are_expected(self, objs, expected_fields,
                                dict_fields=[]):
        """
        Checks that all fields in the objects are from the
        given list of expected fields.
        """
        for o in objs:
            for key in o.keys():
                if key not in dict_fields and key not in expected_fields:
                    raise Exception("Unexpected key '{}' found"
                                    .format(key))

    def load_fields(self, fields_doc="../../etc/fields.yml"):
        """
        Returns a list of fields to expect in the output dictionaries
        and a second list that contains the fields that have a
        dictionary type.

        Reads these lists from the fields documentation.
        """
        def extract_fields(doc_list):
            fields = []
            dictfields = []
            for field in doc_list:
                if field.get("type") == "group":
                    subfields, subdictfields = extract_fields(field["fields"])
                    fields.extend(subfields)
                    dictfields.extend(subdictfields)
                else:
                    fields.append(field["name"])
                    if field.get("type") == "dict":
                        dictfields.append(field["name"])
            return fields, dictfields

        with open(fields_doc, "r") as f:
            doc = yaml.load(f)
            fields = []
            dictfields = []
            for key, value in doc.items():
                if isinstance(value, dict) and \
                        value.get("type") == "group":
                    subfields, subdictfields = extract_fields(value["fields"])
                    fields.extend(subfields)
                    dictfields.extend(subdictfields)
            return fields, dictfields

    def flatten_object(self, obj, dict_fields, prefix=""):
        result = {}
        for key, value in obj.items():
            if isinstance(value, dict) and prefix + key not in dict_fields:
                new_prefix = prefix + key + "."
                result.update(self.flatten_object(value, dict_fields,
                                                  new_prefix))
            else:
                result[prefix + key] = value
        return result

    def copy_files(self, files, source_dir="files/", target_dir=""):
        if target_dir:
            target_dir = os.path.join(self.working_dir, target_dir)
        else:
            target_dir = self.working_dir
        for file_ in files:
            shutil.copy(os.path.join(source_dir, file_),
                        target_dir)

    def output_count(self, pred, output_file=None):
        """
        Returns true if the output line count predicate returns true
        """

        # Init defaults
        if output_file is None:
            output_file = "output/" + self.beat_name

        try:
            with open(os.path.join(self.working_dir, output_file), "r") as f:
                return pred(len([1 for line in f]))
        except IOError:
            return False
